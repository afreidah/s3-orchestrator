// -------------------------------------------------------------------------------
// Object Manager - Type Definition and Shared Helpers
//
// Author: Alex Freidah
//
// ObjectManager struct, constructor, and shared helpers.
// read failover across replicas, broadcast reads during degraded mode, and
// usage limit enforcement on reads and writes. DeleteObjects provides batch
// deletion with concurrent backend I/O.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"io"
	"sync"

	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// managerSpanPrefix is prepended to every OpenTelemetry span name the
// ObjectManager creates so traces clearly distinguish the manager
// layer ("Manager GetObject") from the backend layer ("Backend
// GetObject") in the same end-to-end trace.
const managerSpanPrefix = "Manager "

// ObjectManager handles object-level CRUD operations with read failover,
// broadcast reads during degraded mode, and location caching.
type ObjectManager struct {
	*backendCore
	coord             *writeCoordinator    // write-path helpers shared with BackendManager and MultipartManager
	stores            core.MetadataStore   // direct store access for read paths and quota inspection
	encryptor         *encryption.Encryptor
	cache             *LocationCache
	objectCache       objcache.ObjectCache // nil when object data caching is disabled
	parallelBroadcast bool
	integrityCfg      *syncutil.AtomicConfig[config.IntegrityConfig]
}

// ObjectManagerDeps bundles the dependencies NewObjectManager needs so
// the call signature stays under the parameter-count ceiling.
type ObjectManagerDeps struct {
	Core              *backendCore
	Coord             *writeCoordinator
	Stores            core.MetadataStore
	Encryptor         *encryption.Encryptor
	LocationCache     *LocationCache
	ObjectCache       objcache.ObjectCache
	ParallelBroadcast bool
	IntegrityCfg      *syncutil.AtomicConfig[config.IntegrityConfig]
}

// NewObjectManager creates an ObjectManager sharing the given core
// infrastructure and write coordinator. All dependencies must be
// non-nil; nothing is patched in post-construction.
func NewObjectManager(d *ObjectManagerDeps) *ObjectManager {
	return &ObjectManager{
		backendCore:       d.Core,
		coord:             d.Coord,
		stores:            d.Stores,
		encryptor:         d.Encryptor,
		cache:             d.LocationCache,
		objectCache:       d.ObjectCache,
		parallelBroadcast: d.ParallelBroadcast,
		integrityCfg:      d.IntegrityCfg,
	}
}

// invalidateCache removes a key from the object data cache if caching is enabled.
func (o *ObjectManager) invalidateCache(key string) {
	if o.objectCache != nil {
		o.objectCache.Invalidate(key)
	}
}

// wrapReader returns an io.ReadCloser that reads from r but closes c.
// Used to replace io.NopCloser when the decrypt reader wraps a backend
// response body  -  Close must still reach the original body so the
// underlying HTTP connection is released.
func wrapReader(r io.Reader, c io.Closer) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{Reader: r, Closer: c}
}

// bodyWithCancel wraps body so that its Close also invokes cancel. Used by
// the read path to release per-call timeout contexts when the consumer
// finishes reading the body, instead of letting the timeout fire on its own.
func bodyWithCancel(body io.ReadCloser, cancel func()) io.ReadCloser {
	return &cancellingReadCloser{ReadCloser: body, cancel: cancel}
}

// cancellingReadCloser invokes a cancel func exactly once after the
// wrapped ReadCloser is closed. Idempotent so callers can defer Close
// without worrying about double-cancel.
type cancellingReadCloser struct {
	io.ReadCloser
	cancel func()
	once   sync.Once
}

// Close closes the wrapped ReadCloser and then invokes the cancel func.
// Errors from the underlying Close are returned; cancel cannot fail.
func (c *cancellingReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}

// -------------------------------------------------------------------------
// PRE-FLIGHT CHECKS
// -------------------------------------------------------------------------

// CanAcceptWrite reports whether any backend can accept a write of the given
// size. Used by the HTTP handler to reject uploads before the request body
// is transmitted (Expect: 100-Continue support).
func (o *ObjectManager) CanAcceptWrite(size int64) bool {
	return len(o.eligibleForWrite(1, 0, size)) > 0
}

// BackendCapacityStats returns the current per-backend used/limit byte
// snapshot. Used by the InsufficientStorage error path so the response
// body can name the backends that are at capacity instead of returning
// a generic message. Returns nil on a DB lookup failure so the caller
// can fall back to its terse default.
func (o *ObjectManager) BackendCapacityStats(ctx context.Context) map[string]core.QuotaStat {
	stats, err := o.stores.GetQuotaStats(ctx)
	if err != nil {
		return nil
	}
	return stats
}


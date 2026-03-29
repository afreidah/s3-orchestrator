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
	"io"
	"strings"

	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
)

// ObjectManager handles object-level CRUD operations with read failover,
// broadcast reads during degraded mode, and location caching.
type ObjectManager struct {
	*backendCore
	encryptor         *encryption.Encryptor
	cache             *LocationCache
	objectCache       objcache.ObjectCache // nil when object data caching is disabled
	parallelBroadcast bool
	integrityCfg      func() *config.IntegrityConfig
}

// NewObjectManager creates an ObjectManager sharing the given core infrastructure.
func NewObjectManager(core *backendCore, encryptor *encryption.Encryptor, cache *LocationCache, objectCache objcache.ObjectCache, parallelBroadcast bool, integrityCfg func() *config.IntegrityConfig) *ObjectManager {
	return &ObjectManager{
		backendCore:       core,
		encryptor:         encryptor,
		cache:             cache,
		objectCache:       objectCache,
		parallelBroadcast: parallelBroadcast,
		integrityCfg:      integrityCfg,
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
// response body — Close must still reach the original body so the
// underlying HTTP connection is released.
func wrapReader(r io.Reader, c io.Closer) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{Reader: r, Closer: c}
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

// splitInternalKey extracts the bucket and user-facing key from the internal
// key format "bucket/userkey". Used for notification event payloads so the
// event key matches what the client used, not the internal prefix.
func splitInternalKey(internalKey string) (bucket, key string) {
	bucket, key, _ = strings.Cut(internalKey, "/")
	return bucket, key
}

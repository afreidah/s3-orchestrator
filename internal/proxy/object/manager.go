// -------------------------------------------------------------------------------
// Object Manager - Type Definition, Constructor, and Pre-flight Checks
//
// Author: Alex Freidah
//
// Manager type / Deps / New plus the small shared helpers and pre-flight
// readiness probes (CanAcceptWrite, BackendCapacityStats, ObjectExists)
// the transport layer consults before deciding to accept a request. The
// per-operation orchestration lives in the sibling files: get.go,
// head.go, list.go, range.go, integrity_reader.go for the read paths;
// put.go, copy.go, delete.go, mutation_finalize.go, materialize.go for
// the write paths.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy/readpath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// managerSpanPrefix is prepended to every OpenTelemetry span name the
// Manager creates so traces clearly distinguish the manager
// layer ("Manager GetObject") from the backend layer ("Backend
// GetObject") in the same end-to-end trace.
const managerSpanPrefix = "Manager "

// Stores is the narrow persistence surface object.Manager needs: object
// CRUD + list, plus quota stats. Declared locally so the manager does
// not pull in the full MetadataStore.
type Stores interface {
	core.ObjectStore
	core.QuotaStore
}

// Manager handles object-level CRUD operations with read failover,
// broadcast reads during degraded mode, and location caching.
type Manager struct {
	core              ObjectCore        // infrastructure subset: backends, usage, timeout, eligibility, error classification, metrics
	coord             ObjectCoordinator // write-path helpers shared with BackendManager and MultipartManager
	stores            Stores            // direct store access for read paths and quota inspection
	encryptor         *encryption.Encryptor
	cache             *LocationCache
	objectCache       objcache.ObjectCache // nil when object data caching is disabled
	parallelBroadcast bool
	integrityCfg      *syncutil.AtomicConfig[config.IntegrityConfig]
	failover          *readpath.Failover // per-key read failover + degraded-mode broadcast orchestrator
	log               *slog.Logger
}

// Deps bundles the dependencies New needs so the call signature stays
// under the parameter-count ceiling. Core and Coord are
// consumer-declared interfaces; the concrete *infra.Core and
// *writepath.Coordinator that BackendManager builds satisfy them
// implicitly.
type Deps struct {
	Core              ObjectCore
	Coord             ObjectCoordinator
	Stores            Stores
	Encryptor         *encryption.Encryptor
	LocationCache     *LocationCache
	ObjectCache       objcache.ObjectCache
	ParallelBroadcast bool
	// DegradedBroadcastParallelism caps concurrent probes during
	// parallel degraded-mode broadcasts. 0 = uncapped.
	DegradedBroadcastParallelism int
	// DisableDegradedReads makes the degraded path fail fast instead of broadcasting.
	DisableDegradedReads bool
	IntegrityCfg         *syncutil.AtomicConfig[config.IntegrityConfig]
}

// New creates a Manager sharing the given core infrastructure and
// write coordinator. All dependencies must be non-nil; nothing is
// patched in post-construction. The component-scoped logger is built
// in the constructor body per the project's logging convention. The
// read-failover orchestrator is built once and reused for every GET /
// HEAD; it captures the same Core, Stores, LocationCache, and the
// parallelBroadcast flag, so per-call read paths stay short.
func New(d *Deps) *Manager {
	must.NotNil("d", d)
	must.NotNil("d.Core", d.Core)
	must.NotNil("d.Coord", d.Coord)
	must.NotNil("d.Stores", d.Stores)
	must.NotNil("d.LocationCache", d.LocationCache)
	must.NotNil("d.IntegrityCfg", d.IntegrityCfg)
	return &Manager{
		core:              d.Core,
		coord:             d.Coord,
		stores:            d.Stores,
		encryptor:         d.Encryptor,
		cache:             d.LocationCache,
		objectCache:       d.ObjectCache,
		parallelBroadcast: d.ParallelBroadcast,
		integrityCfg:      d.IntegrityCfg,
		failover:          readpath.New(d.Core, d.Stores, d.LocationCache, d.ParallelBroadcast, d.DegradedBroadcastParallelism, !d.DisableDegradedReads),
		log:               slog.Default().With(logfmt.Component("object")),
	}
}

// invalidateObjectCaches drops both the location cache entry (key ->
// backend placement) and the object data cache entry. Every successful
// mutation must clear both; pairing them here keeps a future caller
// from silently invalidating one and leaving the other stale.
func (o *Manager) invalidateObjectCaches(key string) {
	o.cache.Delete(key)
	if o.objectCache != nil {
		o.objectCache.Invalidate(key)
	}
}

// LocationCache returns the location cache the manager holds. Exposed for
// BackendManager and tests so the lifecycle (Close, Clear) can be driven
// from the root package without reaching into the unexported field.
func (o *Manager) LocationCache() *LocationCache {
	return o.cache
}

// -------------------------------------------------------------------------
// PRE-FLIGHT CHECKS
// -------------------------------------------------------------------------

// CanAcceptWrite reports whether any backend can accept a write of the given
// size. Used by the HTTP handler to reject uploads before the request body
// is transmitted (Expect: 100-Continue support).
func (o *Manager) CanAcceptWrite(size int64) bool {
	return len(o.core.EligibleForWrite(1, 0, size)) > 0
}

// BackendCapacityStats returns the current per-backend used/limit byte
// snapshot. Used by the InsufficientStorage error path so the response
// body can name the backends that are at capacity instead of returning
// a generic message. Returns nil on a DB lookup failure so the caller
// can fall back to its terse default.
func (o *Manager) BackendCapacityStats(ctx context.Context) map[string]core.QuotaStat {
	stats, err := o.stores.GetQuotaStats(ctx)
	if err != nil {
		return nil
	}
	return stats
}

// ListObjectsMaxPages caps DB round trips per ListObjects request so a
// single client call cannot drag the database through unbounded scans on
// pathological prefix layouts. Exposed as a var (rather than const) so
// tests can lower it without generating hundreds of mock pages.
var ListObjectsMaxPages = 100

// ObjectExists reports whether at least one location row exists for key.
// Used by the conditional-write path (If-None-Match: *) to fail-fast
// before the body upload. Best-effort: a concurrent racing PUT can land
// between this read and the eventual RecordObject commit, matching AWS
// S3's documented best-effort precondition semantic. ErrObjectNotFound
// is the canonical "no row" signal and is normalised to (false, nil).
func (o *Manager) ObjectExists(ctx context.Context, key string) (bool, error) {
	locs, err := o.stores.GetAllObjectLocations(ctx, key)
	if errors.Is(err, core.ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check object existence: %w", err)
	}
	return len(locs) > 0, nil
}

// -------------------------------------------------------------------------------
// Manager - Multi-Backend Object Storage Manager
//
// Author: Alex Freidah
//
// Core type and constructor for the backend manager. Object CRUD operations are
// in manager_objects.go, multipart operations in manager_multipart.go, quota
// metrics in manager_metrics.go, rebalancing in rebalancer.go, and replication
// in replicator.go.
// -------------------------------------------------------------------------------

// Package proxy is the domain orchestration layer that coordinates
// multi-backend S3 storage. It routes writes, manages failover reads,
// handles multipart uploads, drains backends, and exposes dashboard data.
// Workers receive the Ops interface instead of direct access.
package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// BACKEND MANAGER
// -------------------------------------------------------------------------

// BackendManagerConfig holds the parameters for creating a BackendManager.
// Stores carries the metadata-store contract. Metrics carries the narrow
// proxy.metrics.Deps used by MetricsCollector.
type BackendManagerConfig struct {
	Backends          map[string]backend.ObjectBackend
	Stores            core.MetadataStore
	Metrics           metrics.Deps
	Dashboard         core.MetadataStore
	// PendingEnabled toggles the PUT-before-COMMIT pending-row pattern
	// (write_path.pending_pattern.enabled). When false the manager skips
	// pending-intent inserts and pending-promotion paths and falls back
	// to the legacy cleanup-on-failure flow.
	PendingEnabled bool
	Order             []string
	CacheTTL          time.Duration
	BackendTimeout    time.Duration
	UsageLimits       map[string]core.UsageLimits
	RoutingStrategy   config.RoutingStrategy
	ParallelBroadcast bool                   // fan-out reads in parallel during degraded mode
	Encryptor         *encryption.Encryptor  // nil when encryption is disabled
	CounterBackend    counter.CounterBackend // nil uses LocalCounterBackend
	ObjectCache       objcache.ObjectCache   // nil when object data caching is disabled
	MaxObjectSizes    map[string]int64       // per-backend max object size in bytes (0 = unlimited)
	AdmissionSem      chan struct{}          // shared concurrency semaphore for HTTP + background ops (nil = unlimited)

	// MultipartDEKCacheTTL pegs the lifetime of cached unwrapped DEKs
	// the MultipartManager uses to avoid re-unwrapping the upload-level
	// DEK on every UploadPart. Zero falls back to a 1h default which
	// is shorter than the typical multipart_stale_timeout but long
	// enough to absorb a reasonable upload's part stream.
	MultipartDEKCacheTTL time.Duration

	// ReplicationFactor is invoked by the metrics collector when refreshing
	// the under-replicated-objects gauge. Returns 0 when replication is
	// disabled. Lazy-evaluated so it can resolve the live replicator's
	// configured factor (which is hot-reloadable).
	ReplicationFactor func() int
}

// BackendManager manages multiple storage backends with quota tracking.
// Embeds *backendCore for non-store infrastructure (backends, usage,
// admission, draining, metrics) and holds the per-role store views and
// hot-reloadable configuration. Store-touching write-path helpers are
// methods on *BackendManager (manager_writepath.go); pure infra
// primitives stay on *backendCore.
//
// Workers (rebalancer, replicator, scrubber, ...) are resolved through
// DI at the call site rather than carried on the manager. The dashboard
// aggregator, hot-reload paths, and tests that previously read
// mgr.Replicator etc. now invoke do.Invoke directly.
//
// DrainManager is the one dependency wired post-construction via
// WireDrain. The cycle (drain.Manager needs *BackendManager and
// mgr.MultipartManager) makes constructor injection impractical without
// redesigning drain.Core. Code paths that depend on DrainManager
// (FlushUsage, ClearDrainState) nil-guard the field so a manager
// constructed without WireDrain (every test path that does not need
// drain behavior) remains usable.
type BackendManager struct {
	*backendCore
	stores           core.MetadataStore    // metadata-store dependency
	coord            *writeCoordinator     // shared write-path helpers (also held by ObjectManager and MultipartManager)
	MultipartManager *MultipartManager     // multipart upload lifecycle
	ObjectManager    *ObjectManager        // CRUD, read failover, broadcast reads
	dashboard        *dashboard.Aggregator // web UI data aggregation

	// DrainManager is the single post-construction wiring point. Set by
	// WireDrain after both *BackendManager and *drain.Manager have been
	// constructed (the dependency cycle prevents constructor injection).
	// Nil-able by design; FlushUsage and ClearDrainState guard the
	// access so tests that do not exercise drain behavior need not call
	// WireDrain.
	DrainManager *drain.Manager

	usageFlushCfg syncutil.AtomicConfig[config.UsageFlushConfig]
	lifecycleCfg  syncutil.AtomicConfig[config.LifecycleConfig]
	integrityCfg  *syncutil.AtomicConfig[config.IntegrityConfig] // shared with ObjectManager
}

// WireDrain installs the drain.Manager: stores it on BackendManager so
// dashboard rendering and drain-aware tests can reach it, and points
// backendCore.drainMgr at it so eligibility filters see drain state.
// Called by the drain DI provider after both values exist. The
// dependency cycle between drain.Manager and BackendManager prevents
// passing the drain manager through the constructor.
func (m *BackendManager) WireDrain(d *drain.Manager) {
	m.DrainManager = d
	m.drainMgr = d
}

// validateConfig checks every input the constructor dereferences and
// returns the first violation wrapped with the matching sentinel so
// callers can errors.Is against the typed value. The scope here is
// narrow on purpose: operator-facing config shape (backend ordering,
// minimum backend count, named-backend uniqueness) is validated by
// internal/config.SetDefaultsAndValidate; this function only catches
// the "NewBackendManager would NPE on first use" cases that the config
// validator does not cover (because tests can construct configs that
// bypass it). Negative-duration checks live here rather than in the
// config validator because the relevant fields are also settable from
// test code that does not go through the config path.
// validateConfigErrFmt wraps a required-input sentinel.
// validateConfigDurationErrFmt wraps a negative-duration sentinel with the
// offending value rendered for the operator.
const (
	validateConfigErrFmt         = "BackendManager: %w"
	validateConfigDurationErrFmt = "BackendManager: %w (%s)"
)

func validateConfig(cfg *BackendManagerConfig) error {
	if cfg == nil {
		return fmt.Errorf(validateConfigErrFmt, ErrConfigNil)
	}
	if cfg.Stores == nil {
		return fmt.Errorf(validateConfigErrFmt, ErrStoresRequired)
	}
	if cfg.Dashboard == nil {
		return fmt.Errorf(validateConfigErrFmt, ErrDashboardRequired)
	}
	if cfg.Metrics == nil {
		return fmt.Errorf(validateConfigErrFmt, ErrMetricsRequired)
	}
	if cfg.BackendTimeout < 0 {
		return fmt.Errorf(validateConfigDurationErrFmt, ErrNegativeBackendTimeout, cfg.BackendTimeout)
	}
	if cfg.CacheTTL < 0 {
		return fmt.Errorf(validateConfigDurationErrFmt, ErrNegativeCacheTTL, cfg.CacheTTL)
	}
	if cfg.MultipartDEKCacheTTL < 0 {
		return fmt.Errorf(validateConfigDurationErrFmt, ErrNegativeMultipartDEKCacheTTL, cfg.MultipartDEKCacheTTL)
	}
	return nil
}

// NewBackendManager validates cfg and constructs a BackendManager.
// Returns a typed sentinel error (errors.Is-matchable) for every
// required input or runtime invariant violation so DI startup fails
// fast rather than NPE'ing at first request.
func NewBackendManager(cfg *BackendManagerConfig) (*BackendManager, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	backendNames := make([]string, 0, len(cfg.Backends))
	for name := range cfg.Backends {
		backendNames = append(backendNames, name)
	}

	counters := cfg.CounterBackend
	if counters == nil {
		counters = counter.NewLocalCounterBackend(backendNames)
	}
	usage := counter.NewUsageTracker(counters, cfg.UsageLimits)

	core := &backendCore{
		backends:        cfg.Backends,
		order:           cfg.Order,
		backendTimeout:  cfg.BackendTimeout,
		usage:           usage,
		routingStrategy: cfg.RoutingStrategy,
		maxObjectSizes:  cfg.MaxObjectSizes,
		admissionSem:    cfg.AdmissionSem,
		log:             slog.Default().With(logfmt.Component("backend_manager")),
	}

	dekCacheTTL := cfg.MultipartDEKCacheTTL
	if dekCacheTTL == 0 {
		dekCacheTTL = time.Hour
	}

	coord := newWriteCoordinator(core, cfg.Stores, cfg.PendingEnabled)
	multipartManager := NewMultipartManager(core, coord, cfg.Stores, cfg.Encryptor, cfg.ObjectCache, dekCacheTTL)

	integrityCfg := &syncutil.AtomicConfig[config.IntegrityConfig]{}
	cache := NewLocationCache(cfg.CacheTTL)
	objectManager := NewObjectManager(&ObjectManagerDeps{
		Core:              core,
		Coord:             coord,
		Stores:            cfg.Stores,
		Encryptor:         cfg.Encryptor,
		LocationCache:     cache,
		ObjectCache:       cfg.ObjectCache,
		ParallelBroadcast: cfg.ParallelBroadcast,
		IntegrityCfg:      integrityCfg,
	})

	m := &BackendManager{
		backendCore:      core,
		stores:           cfg.Stores,
		coord:            coord,
		MultipartManager: multipartManager,
		ObjectManager:    objectManager,
		dashboard:        dashboard.New(cfg.Dashboard, usage, cfg.Order),
		integrityCfg:     integrityCfg,
	}

	core.metricsCollector = metrics.New(cfg.Metrics, usage, backendNames, cfg.ReplicationFactor)

	return m, nil
}

// ClearCache removes all entries from the location cache.
func (m *BackendManager) ClearCache() {
	m.ObjectManager.cache.Clear()
}

// ClearDrainState removes all entries from the draining map. Used by tests
// to reset state between runs. No-op when DrainManager has not been
// wired (tests that do not need drain behavior skip WireDrain).
func (m *BackendManager) ClearDrainState() {
	if m.DrainManager == nil {
		return
	}
	m.DrainManager.ClearState()
}

// AdmissionSem returns the shared admission semaphore, or nil if none is
// configured. The HTTP admission controller should use this channel so that
// HTTP requests and background services share one concurrency budget.
func (m *BackendManager) AdmissionSem() chan struct{} {
	return m.admissionSem
}

// Close stops every background cache eviction goroutine the manager
// owns: the object location cache and the multipart per-upload DEK
// cache. Safe to call multiple times.
func (m *BackendManager) Close() {
	m.ObjectManager.cache.Close()
	if m.MultipartManager != nil && m.MultipartManager.dekCache != nil {
		m.MultipartManager.dekCache.Close()
	}
}

// RecordUsage increments the in-memory usage counters for a backend.
// Exposed for admin operations that bypass the normal manager request path.
func (m *BackendManager) RecordUsage(backendName string, apiCalls, egress, ingress int64) {
	m.usage.Record(backendName, apiCalls, egress, ingress)
}

// UpdateUsageLimits replaces the per-backend usage limits. Safe to call
// concurrently with request handling.
func (m *BackendManager) UpdateUsageLimits(limits map[string]core.UsageLimits) {
	m.usage.UpdateLimits(limits)
}

// FlushUsage flushes accumulated in-memory usage counters to the database.
// Backends that have completed draining are skipped because their DB
// records (including backend_usage) have been removed. When DrainManager
// has not been wired (tests that do not exercise drain behavior) the
// skip set is empty and every backend's counters flush.
func (m *BackendManager) FlushUsage(ctx context.Context) error {
	var skip map[string]bool
	if m.DrainManager != nil {
		skip = m.DrainManager.CompletedBackends()
	}
	return m.usage.FlushUsage(ctx, m.stores, skip)
}

// RedisCounterConfigured returns true when the counter backend is a Redis
// backend, regardless of health status. Used by the flush service to decide
// whether an advisory lock is needed  -  the lock must be held even during
// fallback to prevent double-counting when Redis recovers mid-flush.
func (m *BackendManager) RedisCounterConfigured() bool {
	_, ok := m.usage.Backend().(*counter.RedisCounterBackend)
	return ok
}

// -------------------------------------------------------------------------
// CONFIG ACCESSORS
// -------------------------------------------------------------------------

// SetUsageFlushConfig atomically stores the usage flush configuration.
func (m *BackendManager) SetUsageFlushConfig(cfg *config.UsageFlushConfig) {
	m.usageFlushCfg.Store(cfg)
}

// UsageFlushConfig returns the current usage flush configuration.
func (m *BackendManager) UsageFlushConfig() *config.UsageFlushConfig {
	return m.usageFlushCfg.Load()
}

// SetLifecycleConfig atomically stores the lifecycle configuration.
func (m *BackendManager) SetLifecycleConfig(cfg *config.LifecycleConfig) {
	m.lifecycleCfg.Store(cfg)
}

// LifecycleConfig returns the current lifecycle configuration.
func (m *BackendManager) LifecycleConfig() *config.LifecycleConfig {
	return m.lifecycleCfg.Load()
}

// SetIntegrityConfig atomically stores the integrity configuration. The
// scrubber's own SetConfig is invoked separately by the caller (serve)
// since the scrubber is a top-level DI service after #676 B.
func (m *BackendManager) SetIntegrityConfig(cfg *config.IntegrityConfig) {
	m.integrityCfg.Store(cfg)
}

// IntegrityConfig returns the current integrity configuration.
func (m *BackendManager) IntegrityConfig() *config.IntegrityConfig {
	return m.integrityCfg.Load()
}

// NearUsageLimit returns true if any backend is approaching its usage limits.
func (m *BackendManager) NearUsageLimit(threshold float64) bool {
	return m.usage.NearLimit(threshold)
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// SyncBackend scans a backend's S3 bucket and imports pre-existing
// objects into the proxy database. Objects already tracked for the
// backend are skipped. knownBuckets is the full list of configured
// virtual bucket names, used to distinguish objects belonging to other
// buckets from externally-uploaded objects that need the bucket prefix
// prepended. Returns counts of imported vs skipped objects.
func (m *BackendManager) SyncBackend(ctx context.Context, backendName, bucket string, knownBuckets []string) (imported, skipped int, err error) {
	s3b, err := m.resolveS3Backend(backendName)
	if err != nil {
		return 0, 0, err
	}

	m.Log().InfoContext(ctx, "starting backend sync", "backend", backendName, "bucket", bucket)

	bucketPrefix := internalkey.Prefix(bucket)
	otherPrefixes := siblingPrefixes(knownBuckets, bucket)
	var apiPages int64

	err = s3b.ListObjects(ctx, "", func(objects []backend.ListedObject) error {
		apiPages++
		pImported, pSkipped, err := m.importSyncPage(ctx, backendName, bucketPrefix, otherPrefixes, objects)
		imported += pImported
		skipped += pSkipped
		return err
	})

	// Record ListObjectsV2 API calls against the backend's usage quota:
	// each page is one API request to the backend provider.
	if apiPages > 0 {
		m.usage.Record(backendName, apiPages, 0, 0)
	}
	if err != nil {
		return imported, skipped, err
	}

	m.Log().InfoContext(ctx, "backend sync complete", "backend", backendName, "bucket", bucket,
		"imported", imported, "skipped", skipped)
	return imported, skipped, nil
}

// importSyncPage processes one page of backend ListObjects results,
// importing objects that belong to bucket and skipping those that fall
// inside sibling virtual buckets sharing the same backend.
func (m *BackendManager) importSyncPage(
	ctx context.Context,
	backendName, bucketPrefix string,
	otherPrefixes []string,
	objects []backend.ListedObject,
) (imported, skipped int, err error) {
	for _, obj := range objects {
		key, ok := normalizeSyncKey(obj.Key, bucketPrefix, otherPrefixes)
		if !ok {
			continue
		}
		inserted, importErr := m.stores.ImportObject(ctx, key, backendName, obj.SizeBytes)
		if importErr != nil {
			return imported, skipped, fmt.Errorf("failed to import %s: %w", obj.Key, importErr)
		}
		if inserted {
			imported++
		} else {
			skipped++
		}
	}
	return imported, skipped, nil
}

// normalizeSyncKey returns the storage key to import, plus ok=false when
// the object belongs to a sibling bucket and should be skipped. Keys
// without any known prefix are treated as externally-uploaded objects
// and get the target bucket's prefix prepended.
func normalizeSyncKey(rawKey, bucketPrefix string, otherPrefixes []string) (string, bool) {
	if strings.HasPrefix(rawKey, bucketPrefix) {
		return rawKey, true
	}
	for _, p := range otherPrefixes {
		if strings.HasPrefix(rawKey, p) {
			return "", false
		}
	}
	return bucketPrefix + rawKey, true
}

// makeReconcileDeleter composes the object_locations row delete with a
// cleanup_queue sweep so stale queue entries pointing at the same key
// are removed in lockstep. Without the sweep, queue rows for a key the
// backend no longer holds keep retrying DeleteObject (which 404s) until
// they exhaust attempts and bloat the queue. The sweep failure is best-
// effort: if the cleanup store call errors, the metadata delete still
// stands and the next reconcile pass will sweep the orphan rows. We
// log but do not propagate.
func (m *BackendManager) makeReconcileDeleter() deleterFn {
	return func(ctx context.Context, key, backendName string) error {
		if err := m.stores.DeleteObjectLocation(ctx, key, backendName); err != nil {
			return err
		}
		if _, err := m.stores.SweepStaleCleanupQueueRows(ctx, key, backendName); err != nil {
			m.Log().WarnContext(ctx, "failed to sweep cleanup_queue rows for stale key",
				slog.String("key", key), slog.String("backend", backendName), "error", err)
		}
		return nil
	}
}

// ReconcileBackend reconciles a single backend against the metadata store
// using a bounded-memory sorted-merge: both sides are walked in lex key
// order and diffed in lockstep. The S3 walk and DB cursor each cap their
// in-flight buffer, so memory is independent of object count.
//
// Behaviour: imports keys present on the backend but not in the DB, and
// deletes DB rows whose keys are no longer on the backend. Keys owned by
// sibling virtual buckets stored on the same backend are left alone in
// both directions  -  sibling buckets are reconciled by their own pass.
func (m *BackendManager) ReconcileBackend(ctx context.Context, backendName, bucket string, knownBuckets []string) (*worker.ReconcileResult, error) {
	s3b, err := m.resolveS3Backend(backendName)
	if err != nil {
		return nil, err
	}

	bucketPrefix := internalkey.Prefix(bucket)
	otherPrefixes := siblingPrefixes(knownBuckets, bucket)

	var apiPages int64
	s3 := newS3KeyStream(ctx, s3b, bucketPrefix, otherPrefixes, &apiPages)
	defer s3.stop()

	dbIter := newDBCursorStream(m.stores, backendName, bucketPrefix, otherPrefixes)
	defer dbIter.stop()

	res := &reconcileResult{}
	mergeErr := reconcileSorted(
		ctx, s3, dbIter,
		importHandler(m.Log(), backendName, m.stores.ImportObject, res),
		deleteHandler(m.Log(), backendName, m.makeReconcileDeleter(), res),
	)

	if pages := atomic.LoadInt64(&apiPages); pages > 0 {
		m.usage.Record(backendName, pages, 0, 0)
	}
	if mergeErr != nil {
		return &worker.ReconcileResult{BackendsScanned: 1, Imported: int(res.imported), Removed: int(res.removed)},
			fmt.Errorf("reconcile %s: %w", backendName, mergeErr)
	}

	return &worker.ReconcileResult{
		BackendsScanned: 1,
		Imported:        int(res.imported),
		Removed:         int(res.removed),
	}, nil
}

// resolveS3Backend unwraps any decorators (circuit breaker etc.) and
// returns the underlying lister, which must support the streaming
// ListObjects API the reconciler drives. The interface return makes the
// dependency narrow so tests can substitute a fake.
func (m *BackendManager) resolveS3Backend(name string) (objectLister, error) {
	be, err := m.GetBackend(name)
	if err != nil {
		return nil, err
	}
	inner := be
	for {
		u, ok := inner.(interface{ Unwrap() backend.ObjectBackend })
		if !ok {
			break
		}
		inner = u.Unwrap()
	}
	lister, ok := inner.(objectLister)
	if !ok {
		return nil, fmt.Errorf("backend %s does not support listing", name)
	}
	return lister, nil
}

// siblingPrefixes returns the bucket-prefix list (each suffixed with '/')
// for every known bucket except the one currently being reconciled.
func siblingPrefixes(knownBuckets []string, current string) []string {
	out := make([]string, 0, len(knownBuckets))
	for _, b := range knownBuckets {
		if b != current {
			out = append(out, b+"/")
		}
	}
	return out
}

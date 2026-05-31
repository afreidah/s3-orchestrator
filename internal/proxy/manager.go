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
	"errors"
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
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/reconcile"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// BACKEND MANAGER
// -------------------------------------------------------------------------

// Stores is the narrow persistence surface BackendManager itself touches:
// object import / delete, cleanup-queue sweep, lifecycle expiry listing,
// usage-delta flush, and the multipart count it exposes to the s3api
// transport. Sub-managers (object, writepath, multipart, readpath)
// receive their own narrower role-composite interfaces through their
// constructors; the *core.MetadataStore handed in via BackendManagerConfig
// is the composition-root concrete that satisfies all of them.
type Stores interface {
	core.ObjectStore
	core.CleanupStore
	core.ExpiredObjectsLister
	core.UsageFlusher
	core.MultipartStore
}

// BackendManagerConfig holds the parameters for creating a BackendManager.
// Stores remains core.MetadataStore because BackendManager is the proxy
// subtree's composition root — it routes the concrete store into the
// narrow interfaces each sub-manager declares. Metrics carries the narrow
// proxy.metrics.Deps used by MetricsCollector.
type BackendManagerConfig struct {
	Backends  map[string]backend.ObjectBackend
	Stores    core.MetadataStore
	Metrics   metrics.Deps
	Dashboard core.DashboardStore
	// PendingEnabled toggles the PUT-before-COMMIT pending-row pattern
	// (write_path.pending_pattern.enabled). When false the manager skips
	// pending-intent inserts and pending-promotion paths and falls back
	// to the legacy cleanup-on-failure flow.
	PendingEnabled    bool
	Order             []string
	CacheTTL          time.Duration
	BackendTimeout    time.Duration
	UsageLimits       map[string]core.UsageLimits
	RoutingStrategy   config.RoutingStrategy
	ParallelBroadcast bool // fan-out reads in parallel during degraded mode
	// DegradedBroadcastParallelism caps concurrent probes during a
	// parallel degraded-mode broadcast. 0 = no cap (every backend
	// probed at once, the historical behaviour).
	DegradedBroadcastParallelism int
	// DisableDegradedReads opts the read path out of broadcasting on DB outage.
	DisableDegradedReads bool
	Encryptor                    *encryption.Encryptor  // nil when encryption is disabled
	CounterBackend               counter.CounterBackend // nil uses LocalCounterBackend
	ObjectCache                  objcache.ObjectCache   // nil when object data caching is disabled
	MaxObjectSizes               map[string]int64       // per-backend max object size in bytes (0 = unlimited)
	// AdmissionSem is the shared concurrency semaphore for write-class
	// traffic. In split mode (MaxConcurrentReads + MaxConcurrentWrites)
	// it is sized to MaxConcurrentWrites and is shared between HTTP
	// writes and all background workers; reads run on a separate sem
	// created in transport/httpserver/routes.go. In merged mode
	// (MaxConcurrentRequests only) it is the global pool for every HTTP
	// request and every background worker. nil disables admission entirely
	// (no cap installed). See admissionSemFor in internal/di/backend.go
	// for the sizing rules and #835 for the documentation rationale.
	AdmissionSem chan struct{}

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
// Embeds *infra.Core for non-store infrastructure (backends, usage,
// admission, draining, metrics) and holds the per-role store views and
// hot-reloadable configuration. Store-touching write-path helpers are
// methods on *BackendManager (manager_writepath.go); pure infra
// primitives stay on *infra.Core.
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
	*infra.Core
	stores           Stores                 // narrow store-role view; see Stores interface above
	coord            *writepath.Coordinator // shared write-path helpers (also held by object.Manager and MultipartManager)
	MultipartManager *multipart.Manager     // multipart upload lifecycle
	ObjectManager    *object.Manager        // CRUD, read failover, broadcast reads
	dashboard        *dashboard.Aggregator  // web UI data aggregation

	// DrainManager is the single post-construction wiring point. Set by
	// WireDrain after both *BackendManager and *drain.Manager have been
	// constructed (the dependency cycle prevents constructor injection).
	// Nil-able by design; FlushUsage and ClearDrainState guard the
	// access so tests that do not exercise drain behavior need not call
	// WireDrain.
	DrainManager *drain.Manager

	usageFlushCfg syncutil.AtomicConfig[config.UsageFlushConfig]
	lifecycleCfg  syncutil.AtomicConfig[config.LifecycleConfig]
	integrityCfg  *syncutil.AtomicConfig[config.IntegrityConfig] // shared with object.Manager
}

// WireDrain installs the drain.Manager: stores it on BackendManager so
// dashboard rendering and drain-aware tests can reach it, and points
// backendCore.drainMgr at it so eligibility filters see drain state.
// Called by the drain DI provider after both values exist. The
// dependency cycle between drain.Manager and BackendManager prevents
// passing the drain manager through the constructor.
func (m *BackendManager) WireDrain(d *drain.Manager) {
	m.DrainManager = d
	m.SetDrainChecker(d)
}

// NewBackendManager constructs a BackendManager. Required dependencies
// (cfg, Stores, Dashboard, Metrics) panic via must.NotNil at
// construction so a wiring bug surfaces immediately at DI assembly
// rather than NPE'ing N call frames deep on the first request. Numeric
// config invariants (negative timeouts, ordering rules) are the config
// validator's responsibility; the constructor trusts the values it
// receives.
func NewBackendManager(cfg *BackendManagerConfig) *BackendManager {
	must.NotNil("cfg", cfg)
	must.NotNil("cfg.Stores", cfg.Stores)
	must.NotNil("cfg.Dashboard", cfg.Dashboard)
	must.NotNil("cfg.Metrics", cfg.Metrics)

	backendNames := make([]string, 0, len(cfg.Backends))
	for name := range cfg.Backends {
		backendNames = append(backendNames, name)
	}

	counters := cfg.CounterBackend
	if counters == nil {
		counters = counter.NewLocalCounterBackend(backendNames)
	}
	usage := counter.NewUsageTracker(counters, cfg.UsageLimits)

	c := infra.New(&infra.Config{
		Backends:        cfg.Backends,
		Order:           cfg.Order,
		BackendTimeout:  cfg.BackendTimeout,
		Usage:           usage,
		RoutingStrategy: cfg.RoutingStrategy,
		MaxObjectSizes:  cfg.MaxObjectSizes,
		AdmissionSem:    cfg.AdmissionSem,
		Log:             slog.Default().With(logfmt.Component("backend_manager")),
	})

	dekCacheTTL := cfg.MultipartDEKCacheTTL
	if dekCacheTTL == 0 {
		dekCacheTTL = time.Hour
	}

	coord := writepath.New(c, cfg.Stores, cfg.PendingEnabled)

	// Shared with object.Manager so SIGHUP-driven integrity config
	// changes flip both write paths atomically. Multipart's nil-safe
	// hasher gate (#916) reads through this pointer on each Complete.
	integrityCfg := &syncutil.AtomicConfig[config.IntegrityConfig]{}
	multipartManager := multipart.New(c, coord, cfg.Stores, cfg.Encryptor, cfg.ObjectCache, dekCacheTTL, integrityCfg)

	cache := object.NewLocationCache(cfg.CacheTTL)
	objectManager := object.New(&object.Deps{
		Core:                         c,
		Coord:                        coord,
		Stores:                       cfg.Stores,
		Encryptor:                    cfg.Encryptor,
		LocationCache:                cache,
		ObjectCache:                  cfg.ObjectCache,
		ParallelBroadcast:            cfg.ParallelBroadcast,
		DegradedBroadcastParallelism: cfg.DegradedBroadcastParallelism,
		DisableDegradedReads:         cfg.DisableDegradedReads,
		IntegrityCfg:                 integrityCfg,
	})

	m := &BackendManager{
		Core:             c,
		stores:           cfg.Stores,
		coord:            coord,
		MultipartManager: multipartManager,
		ObjectManager:    objectManager,
		dashboard:        dashboard.New(cfg.Dashboard, usage, cfg.Order),
		integrityCfg:     integrityCfg,
	}

	c.SetMetricsCollector(metrics.New(cfg.Metrics, usage, backendNames, cfg.ReplicationFactor))

	return m
}

// ClearCache removes all entries from the location cache.
func (m *BackendManager) ClearCache() {
	m.ObjectManager.LocationCache().Clear()
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
	return m.Core.AdmissionSem()
}

// Close stops every background cache eviction goroutine the manager
// owns: the object location cache and the multipart per-upload DEK
// cache. Safe to call multiple times.
func (m *BackendManager) Close() {
	m.ObjectManager.LocationCache().Close()
	if m.MultipartManager != nil {
		m.MultipartManager.Close()
	}
}

// RecordUsage increments the in-memory usage counters for a backend.
// Exposed for admin operations that bypass the normal manager request path.
func (m *BackendManager) RecordUsage(backendName string, apiCalls, egress, ingress int64) {
	m.Usage().Record(backendName, apiCalls, egress, ingress)
}

// UpdateUsageLimits replaces the per-backend usage limits. Safe to call
// concurrently with request handling.
func (m *BackendManager) UpdateUsageLimits(limits map[string]core.UsageLimits) {
	m.Usage().UpdateLimits(limits)
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
	return m.Usage().FlushUsage(ctx, m.stores, skip)
}

// RedisCounterConfigured returns true when the counter backend is a Redis
// backend, regardless of health status. Used by the flush service to decide
// whether an advisory lock is needed  -  the lock must be held even during
// fallback to prevent double-counting when Redis recovers mid-flush.
func (m *BackendManager) RedisCounterConfigured() bool {
	_, ok := m.Usage().Backend().(*counter.RedisCounterBackend)
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
	return m.Usage().NearLimit(threshold)
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
	otherPrefixes := reconcile.SiblingPrefixes(knownBuckets, bucket)
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
		m.Acct().APICalls(backendName, apiPages)
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
func (m *BackendManager) makeReconcileDeleter() reconcile.DeleterFn {
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
	otherPrefixes := reconcile.SiblingPrefixes(knownBuckets, bucket)

	var apiPages int64
	s3 := reconcile.NewS3KeyStream(ctx, s3b, bucketPrefix, otherPrefixes, &apiPages)
	defer s3.Stop()

	dbIter := reconcile.NewDBCursorStream(m.stores, backendName, bucketPrefix, otherPrefixes)
	defer dbIter.Stop()

	res := &reconcile.Result{}
	mergeErr := reconcile.Sorted(
		ctx, s3, dbIter,
		reconcile.ImportHandler(m.Log(), backendName, m.stores.ImportObject, res),
		reconcile.DeleteHandler(m.Log(), backendName, m.makeReconcileDeleter(), res),
	)

	if pages := atomic.LoadInt64(&apiPages); pages > 0 {
		m.Acct().APICalls(backendName, pages)
	}
	if mergeErr != nil {
		return &worker.ReconcileResult{BackendsScanned: 1, Imported: int(res.Imported), Removed: int(res.Removed)},
			fmt.Errorf("reconcile %s: %w", backendName, mergeErr)
	}

	return &worker.ReconcileResult{
		BackendsScanned: 1,
		Imported:        int(res.Imported),
		Removed:         int(res.Removed),
	}, nil
}

// resolveS3Backend unwraps any decorators (circuit breaker etc.) and
// returns the underlying lister, which must support the streaming
// ListObjects API the reconciler drives. The interface return makes the
// dependency narrow so tests can substitute a fake.
func (m *BackendManager) resolveS3Backend(name string) (reconcile.ObjectLister, error) {
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
	lister, ok := inner.(reconcile.ObjectLister)
	if !ok {
		return nil, fmt.Errorf("backend %s does not support listing", name)
	}
	return lister, nil
}

// -------------------------------------------------------------------------
// STORE-ROLE ACCESSORS
// -------------------------------------------------------------------------

// CountActiveMultipartUploads delegates to the multipart store. Exposed
// for the s3api bucket-delete pre-check so the transport layer does not
// need to reach into the persistence layer directly.
func (m *BackendManager) CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error) {
	return m.stores.CountActiveMultipartUploads(ctx, bucketPrefix)
}

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// SelectReplicaTarget picks a target backend for a replication copy using
// the same routing strategy as normal writes. Excludes backends that
// already hold a copy of the object.
func (m *BackendManager) SelectReplicaTarget(ctx context.Context, size int64, exclusion map[string]bool) (string, error) {
	eligible := m.EligibleForWrite(1, 0, size)
	filtered := make([]string, 0, len(eligible))
	for _, name := range eligible {
		if !exclusion[name] {
			filtered = append(filtered, name)
		}
	}
	if len(filtered) == 0 {
		return "", nil
	}
	name, err := m.coord.SelectBackendForWrite(ctx, size, filtered)
	if errors.Is(err, core.ErrNoSpaceAvailable) {
		return "", nil
	}
	return name, err
}

// -------------------------------------------------------------------------
// PASS-THROUGHS
// -------------------------------------------------------------------------

// DeleteOrEnqueue forwards to the write coordinator. Worker and drain
// interfaces accept *BackendManager and call DeleteOrEnqueue on it; the
// implementation lives on the coordinator now, but the call site shape
// stays the same.
func (m *BackendManager) DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64) {
	m.coord.DeleteOrEnqueue(ctx, be, backendName, key, reason, sizeBytes)
}

// MoveObject forwards to the write coordinator's shared move primitive
// (#924). Drain and rebalancer both pass their narrow consumer
// interface (drain.Core, worker.DataMover) here so the StreamCopy +
// MoveObjectLocation CAS + orphan-cleanup branches + source-delete
// accounting all funnel through one implementation - the same way
// DeleteOrEnqueue does.
func (m *BackendManager) MoveObject(ctx context.Context, req *writepath.MoveRequest) (int64, error) {
	return m.coord.MoveObject(ctx, req)
}

// GetDashboardData delegates to the dashboard.Aggregator and enriches the
// result with drain status and circuit-breaker health from the
// BackendManager's in-memory state.
func (m *BackendManager) GetDashboardData(ctx context.Context) (*dashboard.Data, error) {
	data, err := m.dashboard.GetData(ctx)
	if err != nil {
		return nil, err
	}

	data.DrainingBackends = make(map[string]drain.Progress)
	for _, name := range m.BackendOrder() {
		if !m.IsDraining(name) {
			continue
		}
		progress, err := m.DrainManager.GetDrainProgress(ctx, name)
		if err == nil {
			data.DrainingBackends[name] = *progress
		}
	}

	data.UnhealthyBackends = make(map[string]bool)
	for name, be := range m.Backends() {
		if cb, ok := be.(*backend.CircuitBreakerBackend); ok && !cb.IsHealthy() {
			data.UnhealthyBackends[name] = true
		}
	}

	return data, nil
}

// GetDirectoryChildren delegates to the dashboard.Aggregator.
func (m *BackendManager) GetDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.DirectoryListResult, error) {
	return m.dashboard.GetDirectoryChildren(ctx, prefix, startAfter, maxKeys)
}

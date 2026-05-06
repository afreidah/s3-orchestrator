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
	"crypto/rand"
	"encoding/hex"
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
// Stores carries the narrow per-role store interfaces instead of one "god"
// store; Metrics and Dashboard carry the narrow views used by
// MetricsCollector and DashboardAggregator respectively.
type BackendManagerConfig struct {
	Backends          map[string]backend.ObjectBackend
	Stores            Stores
	Metrics           metrics.Deps
	Dashboard         core.DashboardStore
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
// admission, draining, metrics) and holds the per-role store views,
// workers, and hot-reloadable configuration. Store-touching write-path
// helpers are methods on *BackendManager (manager_writepath.go); pure
// infra primitives stay on *backendCore.
type BackendManager struct {
	*backendCore
	stores           Stores                // per-role store views
	MultipartManager *MultipartManager     // multipart upload lifecycle
	ObjectManager    *ObjectManager        // CRUD, read failover, broadcast reads
	dashboard        *dashboard.Aggregator // web UI data aggregation

	// The following worker handles are wired post-construction by the
	// per-worker DI providers (#676 B). Production code resolves workers
	// through DI; these fields exist as convenience handles for the
	// dashboard, tests, and config-reload paths that already hold a
	// *BackendManager. Treat them as nil-able when accessing outside of
	// fully-DI-wired contexts.
	Rebalancer             *worker.Rebalancer
	Replicator             *worker.Replicator
	OverReplicationCleaner *worker.OverReplicationCleaner
	CleanupWorker          *worker.CleanupWorker
	PendingReaper          *worker.PendingReaper
	Scrubber               *worker.Scrubber
	DrainManager           *drain.Manager

	usageFlushCfg syncutil.AtomicConfig[config.UsageFlushConfig]
	lifecycleCfg  syncutil.AtomicConfig[config.LifecycleConfig]
	integrityCfg  syncutil.AtomicConfig[config.IntegrityConfig]
}

// WireDrain installs the drain.Manager: stores it on BackendManager so
// dashboard rendering and drain-aware tests can reach it, and points
// backendCore.drainMgr at it so eligibility filters see drain state.
// Called by the drain DI provider after both values exist.
func (m *BackendManager) WireDrain(d *drain.Manager) {
	m.DrainManager = d
	m.drainMgr = d
}

// NewBackendManager creates a new backend manager with the given configuration.
func NewBackendManager(cfg *BackendManagerConfig) *BackendManager {
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
	}

	dekCacheTTL := cfg.MultipartDEKCacheTTL
	if dekCacheTTL == 0 {
		dekCacheTTL = time.Hour
	}
	multipartManager := NewMultipartManager(core, cfg.Encryptor, cfg.ObjectCache, dekCacheTTL)
	cache := NewLocationCache(cfg.CacheTTL)
	// ObjectManager gets a closure for the integrity config so it can read
	// the hot-reloadable value without a circular dependency.
	var m *BackendManager
	objectManager := NewObjectManager(core, cfg.Encryptor, cache, cfg.ObjectCache, cfg.ParallelBroadcast, func() *config.IntegrityConfig {
		if m == nil {
			return nil
		}
		return m.IntegrityConfig()
	})

	m = &BackendManager{
		backendCore:      core,
		stores:           cfg.Stores,
		MultipartManager: multipartManager,
		ObjectManager:    objectManager,
		dashboard:        dashboard.New(cfg.Dashboard, usage, cfg.Order),
	}
	multipartManager.parent = m
	objectManager.parent = m

	core.metricsCollector = metrics.New(cfg.Metrics, usage, backendNames, cfg.ReplicationFactor)

	return m
}

// ClearCache removes all entries from the location cache.
func (m *BackendManager) ClearCache() {
	m.ObjectManager.cache.Clear()
}

// ClearDrainState removes all entries from the draining map. Used by tests
// to reset state between runs.
func (m *BackendManager) ClearDrainState() {
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
// Backends that have completed draining are skipped because their DB records
// (including backend_usage) have been removed.
func (m *BackendManager) FlushUsage(ctx context.Context) error {
	skip := m.DrainManager.CompletedBackends()
	return m.usage.FlushUsage(ctx, m.stores.Usage, skip)
}

// RedisCounterActive returns true when the counter backend is a Redis
// backend that is currently healthy (not in fallback). Used by the flush
// service to decide whether to acquire an advisory lock (only one instance
// should flush Redis->PG via GETSET).
func (m *BackendManager) RedisCounterActive() bool {
	rb, ok := m.usage.Backend().(*counter.RedisCounterBackend)
	return ok && rb.IsHealthy()
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

// GenerateUploadID creates a random hex string for multipart upload IDs.
func GenerateUploadID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

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

	slog.InfoContext(ctx, "starting backend sync", "backend", backendName, "bucket", bucket)

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

	slog.InfoContext(ctx, "backend sync complete", "backend", backendName, "bucket", bucket,
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
		inserted, importErr := m.stores.Object.ImportObject(ctx, key, backendName, obj.SizeBytes)
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
		if err := m.stores.Object.DeleteObjectLocation(ctx, key, backendName); err != nil {
			return err
		}
		if _, err := m.stores.Cleanup.SweepStaleCleanupQueueRows(ctx, key, backendName); err != nil {
			slog.WarnContext(ctx, "failed to sweep cleanup_queue rows for stale key",
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

	dbIter := newDBCursorStream(m.stores.Object, backendName, bucketPrefix, otherPrefixes)
	defer dbIter.stop()

	res := &reconcileResult{}
	mergeErr := reconcileSorted(
		ctx, s3, dbIter,
		importHandler(backendName, m.stores.Object.ImportObject, res),
		deleteHandler(backendName, m.makeReconcileDeleter(), res),
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
	be, err := m.getBackend(name)
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

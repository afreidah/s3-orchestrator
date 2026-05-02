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
	Backends           map[string]backend.ObjectBackend
	Stores             Stores
	Metrics            metrics.Deps
	Dashboard          core.DashboardStore
	Order              []string
	CacheTTL           time.Duration
	BackendTimeout     time.Duration
	UsageLimits        map[string]core.UsageLimits
	RoutingStrategy    config.RoutingStrategy
	ParallelBroadcast  bool                   // fan-out reads in parallel during degraded mode
	Encryptor          *encryption.Encryptor  // nil when encryption is disabled
	CounterBackend     counter.CounterBackend // nil uses LocalCounterBackend
	ObjectCache        objcache.ObjectCache   // nil when object data caching is disabled
	MaxObjectSizes     map[string]int64       // per-backend max object size in bytes (0 = unlimited)
	CleanupConcurrency int                    // parallel cleanup deletions (default: 10)
	AdmissionSem       chan struct{}          // shared concurrency semaphore for HTTP + background ops (nil = unlimited)

	PendingReaperMinAge time.Duration // ignore intents younger than this — guards in-flight PUTs
	PendingReaperBatch  int           // max intents resolved per reaper tick
}

// BackendManager manages multiple storage backends with quota tracking.
// Embeds *backendCore for shared infrastructure (backend map, store, usage,
// timeouts, routing) and adds the S3 API surface, encryption, caching,
// dashboard, and hot-reloadable configuration.
type BackendManager struct {
	*backendCore
	Rebalancer             *worker.Rebalancer             // periodic object distribution
	Replicator             *worker.Replicator             // background replica creation
	OverReplicationCleaner *worker.OverReplicationCleaner // excess copy removal
	CleanupWorker          *worker.CleanupWorker          // retry queue for failed deletions
	PendingReaper          *worker.PendingReaper          // resolves abandoned PUT intents
	Scrubber               *worker.Scrubber               // background integrity verification
	DrainManager           *drain.Manager                 // backend drain and remove operations
	MultipartManager       *MultipartManager              // multipart upload lifecycle
	ObjectManager          *ObjectManager                 // CRUD, read failover, broadcast reads
	dashboard              *dashboard.Aggregator          // web UI data aggregation
	usageFlushCfg          syncutil.AtomicConfig[config.UsageFlushConfig]
	lifecycleCfg           syncutil.AtomicConfig[config.LifecycleConfig]
	integrityCfg           syncutil.AtomicConfig[config.IntegrityConfig]
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
		backends:         cfg.Backends,
		objects:          cfg.Stores.Object,
		quota:            cfg.Stores.Quota,
		multipart:        cfg.Stores.Multipart,
		cleanup:          cfg.Stores.Cleanup,
		pending:          cfg.Stores.Pending,
		lifecycle:        cfg.Stores.Lifecycle,
		backendLifecycle: cfg.Stores.BackendLifecycle,
		usageFlusher:     cfg.Stores.Usage,
		order:            cfg.Order,
		backendTimeout:   cfg.BackendTimeout,
		usage:            usage,
		routingStrategy:  cfg.RoutingStrategy,
		maxObjectSizes:   cfg.MaxObjectSizes,
		admissionSem:     cfg.AdmissionSem,
	}

	cleanupConcurrency := cfg.CleanupConcurrency
	if cleanupConcurrency <= 0 {
		cleanupConcurrency = 10
	}
	cleanupWorker := worker.NewCleanupWorker(core, cfg.Stores.Cleanup, cleanupConcurrency)
	// PendingReaper uses the same admission/data-mover/usage surface as the
	// cleanup worker. The constructor's zero-value fallbacks cover any
	// settings the caller leaves unset; the lifecycle scheduler chooses the
	// tick interval.
	var pendingReaper *worker.PendingReaper
	if cfg.Stores.Pending != nil {
		pendingReaper = worker.NewPendingReaper(core, cfg.Stores.Pending, 0, cfg.PendingReaperMinAge, cfg.PendingReaperBatch)
	}
	multipartManager := NewMultipartManager(core, cfg.Encryptor, cfg.ObjectCache)
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

	rebalancerDeps := &rebalancerStore{ObjectStore: cfg.Stores.Object, QuotaStore: cfg.Stores.Quota}
	replicatorDeps := &replicatorStore{ObjectStore: cfg.Stores.Object, ReplicationStore: cfg.Stores.Replication, QuotaStore: cfg.Stores.Quota}
	overReplicationDeps := &overReplicationStore{ReplicationStore: cfg.Stores.Replication, QuotaStore: cfg.Stores.Quota}

	drainManager := drain.New(
		core,
		cfg.Stores.Object,
		cfg.Stores.Quota,
		cfg.Stores.BackendLifecycle,
		multipartManager.abortMultipartUploadsOnBackend,
		cleanupWorker.ProcessCleanupQueue,
	)
	core.drainMgr = drainManager

	m = &BackendManager{
		backendCore:            core,
		Rebalancer:             worker.NewRebalancer(core, rebalancerDeps),
		Replicator:             worker.NewReplicator(core, replicatorDeps),
		OverReplicationCleaner: worker.NewOverReplicationCleaner(core, overReplicationDeps),
		CleanupWorker:          cleanupWorker,
		PendingReaper:          pendingReaper,
		Scrubber:               worker.NewScrubber(core, cfg.Stores.Integrity, cfg.Encryptor),
		MultipartManager:       multipartManager,
		ObjectManager:          objectManager,
		DrainManager:           drainManager,
		dashboard:              dashboard.New(cfg.Dashboard, usage, cfg.Order),
	}

	core.metricsCollector = metrics.New(cfg.Metrics, usage, backendNames, func() int {
		if rc := m.Replicator.Config(); rc != nil {
			return rc.Factor
		}
		return 0
	})

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

// Close stops the background cache eviction goroutine. Safe to call multiple times.
func (m *BackendManager) Close() {
	m.ObjectManager.cache.Close()
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
	return m.usage.FlushUsage(ctx, m.usageFlusher, skip)
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
// whether an advisory lock is needed — the lock must be held even during
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

// SetIntegrityConfig atomically stores the integrity configuration and
// forwards it to the scrubber worker.
func (m *BackendManager) SetIntegrityConfig(cfg *config.IntegrityConfig) {
	m.integrityCfg.Store(cfg)
	m.Scrubber.SetConfig(cfg)
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

// SyncBackend scans a backend's S3 bucket and imports pre-existing objects into
// the proxy database. Objects already tracked for the backend are skipped.
// knownBuckets is the full list of configured virtual bucket names, used to
// distinguish objects belonging to other buckets from externally-uploaded
// objects that need the bucket prefix prepended.
// Returns counts of imported vs skipped objects.
func (m *BackendManager) SyncBackend(ctx context.Context, backendName, bucket string, knownBuckets []string) (imported, skipped int, err error) {
	s3b, err := m.resolveS3Backend(backendName)
	if err != nil {
		return 0, 0, err
	}

	slog.InfoContext(ctx, "starting backend sync", "backend", backendName, "bucket", bucket)

	bucketPrefix := internalkey.Prefix(bucket)

	// Build a set of other bucket prefixes so we can skip objects that belong
	// to a different virtual bucket.
	otherPrefixes := make([]string, 0, len(knownBuckets))
	for _, b := range knownBuckets {
		if b != bucket {
			otherPrefixes = append(otherPrefixes, b+"/")
		}
	}

	var apiPages int64

	err = s3b.ListObjects(ctx, "", func(objects []backend.ListedObject) error {
		apiPages++
		for _, obj := range objects {
			key := obj.Key

			// Already belongs to this bucket — use as-is.
			if strings.HasPrefix(key, bucketPrefix) {
				// good, keep key
			} else {
				// Check if it belongs to a different virtual bucket — skip.
				belongsToOther := false
				for _, p := range otherPrefixes {
					if strings.HasPrefix(key, p) {
						belongsToOther = true
						break
					}
				}
				if belongsToOther {
					continue
				}
				// Externally-uploaded object without a bucket prefix — prepend.
				key = bucketPrefix + key
			}

			ok, importErr := m.objects.ImportObject(ctx, key, backendName, obj.SizeBytes)
			if importErr != nil {
				return fmt.Errorf("failed to import %s: %w", obj.Key, importErr)
			}
			if ok {
				imported++
			} else {
				skipped++
			}
		}
		return nil
	})

	// Record ListObjectsV2 API calls against the backend's usage quota.
	// Each page is one API request to the backend provider.
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
		if err := m.objects.DeleteObjectLocation(ctx, key, backendName); err != nil {
			return err
		}
		if _, err := m.cleanup.SweepStaleCleanupQueueRows(ctx, key, backendName); err != nil {
			slog.WarnContext(ctx, "Reconcile: failed to sweep cleanup_queue rows for stale key",
				"key", key, "backend", backendName, "error", err)
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
// both directions — sibling buckets are reconciled by their own pass.
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

	dbIter := newDBCursorStream(m.objects, backendName, bucketPrefix, otherPrefixes)
	defer dbIter.stop()

	res := &reconcileResult{}
	mergeErr := reconcileSorted(
		ctx, s3, dbIter,
		importHandler(backendName, m.objects.ImportObject, res),
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

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
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/store"
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
	Metrics            MetricsDeps
	Dashboard          store.DashboardStore
	Order              []string
	CacheTTL           time.Duration
	BackendTimeout     time.Duration
	UsageLimits        map[string]store.UsageLimits
	RoutingStrategy    config.RoutingStrategy
	ParallelBroadcast  bool                   // fan-out reads in parallel during degraded mode
	Encryptor          *encryption.Encryptor  // nil when encryption is disabled
	CounterBackend     counter.CounterBackend // nil uses LocalCounterBackend
	ObjectCache        objcache.ObjectCache   // nil when object data caching is disabled
	MaxObjectSizes     map[string]int64       // per-backend max object size in bytes (0 = unlimited)
	CleanupConcurrency int                    // parallel cleanup deletions (default: 10)
	AdmissionSem       chan struct{}          // shared concurrency semaphore for HTTP + background ops (nil = unlimited)
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
	Scrubber               *worker.Scrubber               // background integrity verification
	DrainManager           *DrainManager                  // backend drain and remove operations
	MultipartManager       *MultipartManager              // multipart upload lifecycle
	ObjectManager          *ObjectManager                 // CRUD, read failover, broadcast reads
	dashboard              *DashboardAggregator           // web UI data aggregation
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

	m = &BackendManager{
		backendCore:            core,
		Rebalancer:             worker.NewRebalancer(core, rebalancerDeps),
		Replicator:             worker.NewReplicator(core, replicatorDeps),
		OverReplicationCleaner: worker.NewOverReplicationCleaner(core, overReplicationDeps),
		CleanupWorker:          cleanupWorker,
		Scrubber:               worker.NewScrubber(core, cfg.Stores.Integrity, cfg.Encryptor),
		MultipartManager:       multipartManager,
		ObjectManager:          objectManager,
		DrainManager: NewDrainManager(core,
			multipartManager.abortMultipartUploadsOnBackend,
			cleanupWorker.ProcessCleanupQueue,
		),
		dashboard: NewDashboardAggregator(cfg.Dashboard, usage, cfg.Order),
	}

	core.metrics = NewMetricsCollector(cfg.Metrics, usage, backendNames, func() int {
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
	m.draining.Range(func(key, _ any) bool {
		m.draining.Delete(key)
		return true
	})
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
func (m *BackendManager) UpdateUsageLimits(limits map[string]store.UsageLimits) {
	m.usage.UpdateLimits(limits)
}

// FlushUsage flushes accumulated in-memory usage counters to the database.
// Backends that have completed draining are skipped because their DB records
// (including backend_usage) have been removed.
func (m *BackendManager) FlushUsage(ctx context.Context) error {
	skip := make(map[string]bool)
	m.draining.Range(func(key, val any) bool {
		state := val.(*drainState)
		select {
		case <-state.done:
			skip[key.(string)] = true
		default:
		}
		return true
	})
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
	be, err := m.getBackend(backendName)
	if err != nil {
		return 0, 0, err
	}

	// Unwrap any circuit breaker wrappers to get the concrete *backend.S3Backend.
	inner := be
	for {
		if u, ok := inner.(interface{ Unwrap() backend.ObjectBackend }); ok {
			inner = u.Unwrap()
		} else {
			break
		}
	}
	s3b, ok := inner.(*backend.S3Backend)
	if !ok {
		return 0, 0, fmt.Errorf("backend %s does not support listing", backendName)
	}

	slog.InfoContext(ctx, "starting backend sync", "backend", backendName, "bucket", bucket)

	bucketPrefix := bucket + "/"

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

// ReconcileBackend performs a full reconciliation for a single backend: lists
// objects on the backend, diffs against DB entries, imports untracked objects,
// and removes stale DB entries. Uses a single ListObjects call per backend.
func (m *BackendManager) ReconcileBackend(ctx context.Context, backendName, bucket string, knownBuckets []string) (*worker.ReconcileResult, error) {
	be, err := m.getBackend(backendName)
	if err != nil {
		return nil, err
	}

	inner := be
	for {
		if u, ok := inner.(interface{ Unwrap() backend.ObjectBackend }); ok {
			inner = u.Unwrap()
		} else {
			break
		}
	}
	s3b, ok := inner.(*backend.S3Backend)
	if !ok {
		return nil, fmt.Errorf("backend %s does not support listing", backendName)
	}

	bucketPrefix := bucket + "/"
	otherPrefixes := make([]string, 0, len(knownBuckets))
	for _, b := range knownBuckets {
		if b != bucket {
			otherPrefixes = append(otherPrefixes, b+"/")
		}
	}

	// Build set of keys that actually exist on the backend
	realKeys := make(map[string]int64) // key -> size
	var apiPages int64
	err = s3b.ListObjects(ctx, "", func(objects []backend.ListedObject) error {
		apiPages++
		for _, obj := range objects {
			key := obj.Key
			if strings.HasPrefix(key, bucketPrefix) {
				// belongs to this bucket
			} else {
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
				key = bucketPrefix + key
			}
			realKeys[key] = obj.SizeBytes
		}
		return nil
	})
	if apiPages > 0 {
		m.usage.Record(backendName, apiPages, 0, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("list objects on %s: %w", backendName, err)
	}

	result := &worker.ReconcileResult{BackendsScanned: 1}

	// Import objects on backend but not in DB
	for key, size := range realKeys {
		imported, importErr := m.objects.ImportObject(ctx, key, backendName, size)
		if importErr != nil {
			slog.WarnContext(ctx, "Reconcile: import failed", "key", key, "backend", backendName, "error", importErr)
			continue
		}
		if imported {
			result.Imported++
		}
	}

	// Remove DB entries not on backend
	dbObjects, err := m.objects.ListObjectsByBackend(ctx, backendName, 100000)
	if err != nil {
		return result, fmt.Errorf("list DB objects for %s: %w", backendName, err)
	}
	for i := range dbObjects {
		if _, exists := realKeys[dbObjects[i].ObjectKey]; !exists {
			if delErr := m.objects.DeleteObjectLocation(ctx, dbObjects[i].ObjectKey, backendName); delErr != nil {
				slog.WarnContext(ctx, "Reconcile: failed to remove stale entry",
					"key", dbObjects[i].ObjectKey, "backend", backendName, "error", delErr)
			} else {
				slog.InfoContext(ctx, "Reconcile: removed stale entry",
					"key", dbObjects[i].ObjectKey, "backend", backendName)
				result.Removed++
			}
		}
	}

	return result, nil
}

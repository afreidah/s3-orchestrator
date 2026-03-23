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
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/worker"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
)

// UsageLimits holds configurable monthly usage limits for a single backend.
// Zero means unlimited for that dimension.
type UsageLimits struct {
	APIRequestLimit  int64
	EgressByteLimit  int64
	IngressByteLimit int64
}

// -------------------------------------------------------------------------
// ERRORS
// -------------------------------------------------------------------------

// -------------------------------------------------------------------------
// BACKEND MANAGER
// -------------------------------------------------------------------------

// BackendManagerConfig holds the parameters for creating a BackendManager.
type BackendManagerConfig struct {
	Backends           map[string]backend.ObjectBackend
	Store              store.MetadataStore
	Order              []string
	CacheTTL           time.Duration
	BackendTimeout     time.Duration
	UsageLimits        map[string]store.UsageLimits
	RoutingStrategy    string
	ParallelBroadcast  bool                   // fan-out reads in parallel during degraded mode
	Encryptor          *encryption.Encryptor  // nil when encryption is disabled
	CounterBackend     counter.CounterBackend // nil uses LocalCounterBackend
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
	usageFlushCfg          atomic.Pointer[config.UsageFlushConfig]
	lifecycleCfg           atomic.Pointer[config.LifecycleConfig]
	integrityCfg           atomic.Pointer[config.IntegrityConfig]
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
		store:           cfg.Store,
		order:           cfg.Order,
		backendTimeout:  cfg.BackendTimeout,
		usage:           usage,
		routingStrategy: cfg.RoutingStrategy,
		admissionSem:    cfg.AdmissionSem,
	}

	cleanupConcurrency := cfg.CleanupConcurrency
	if cleanupConcurrency <= 0 {
		cleanupConcurrency = 10
	}
	cleanupWorker := worker.NewCleanupWorker(core, cleanupConcurrency)
	multipartManager := NewMultipartManager(core, cfg.Encryptor)
	cache := NewLocationCache(cfg.CacheTTL)
	// ObjectManager gets a closure for the integrity config so it can read
	// the hot-reloadable value without a circular dependency.
	var m *BackendManager
	objectManager := NewObjectManager(core, cfg.Encryptor, cache, cfg.ParallelBroadcast, func() *config.IntegrityConfig {
		if m == nil {
			return nil
		}
		return m.IntegrityConfig()
	})

	m = &BackendManager{
		backendCore:            core,
		Rebalancer:             worker.NewRebalancer(core),
		Replicator:             worker.NewReplicator(core),
		OverReplicationCleaner: worker.NewOverReplicationCleaner(core),
		CleanupWorker:          cleanupWorker,
		Scrubber:               worker.NewScrubber(core, cfg.Encryptor),
		MultipartManager:       multipartManager,
		ObjectManager:          objectManager,
		DrainManager: NewDrainManager(core,
			multipartManager.abortMultipartUploadsOnBackend,
			cleanupWorker.ProcessCleanupQueue,
		),
		dashboard: NewDashboardAggregator(cfg.Store, usage, cfg.Order),
	}

	core.metrics = NewMetricsCollector(cfg.Store, usage, backendNames, func() int {
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
	return m.usage.FlushUsage(ctx, m.store, skip)
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

	slog.InfoContext(ctx, "Starting backend sync", "backend", backendName, "bucket", bucket)

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

			ok, importErr := m.store.ImportObject(ctx, key, backendName, obj.SizeBytes)
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

	slog.InfoContext(ctx, "Backend sync complete", "backend", backendName, "bucket", bucket,
		"imported", imported, "skipped", skipped)
	return imported, skipped, nil
}

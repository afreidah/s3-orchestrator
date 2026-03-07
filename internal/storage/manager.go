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

package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

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

var (
	// ErrInsufficientStorage is returned when no backend has enough quota.
	ErrInsufficientStorage = &S3Error{StatusCode: 507, Code: "InsufficientStorage", Message: "no backend has sufficient quota"}

	// ErrUsageLimitExceeded is returned when all backends holding an object
	// have exceeded their monthly usage limits.
	ErrUsageLimitExceeded = &S3Error{StatusCode: 429, Code: "SlowDown", Message: "monthly usage limit exceeded for all backends holding this object"}
)

// -------------------------------------------------------------------------
// BACKEND MANAGER
// -------------------------------------------------------------------------

// BackendManagerConfig holds the parameters for creating a BackendManager.
type BackendManagerConfig struct {
	Backends          map[string]ObjectBackend
	Store             MetadataStore
	Order             []string
	CacheTTL          time.Duration
	BackendTimeout    time.Duration
	UsageLimits       map[string]UsageLimits
	RoutingStrategy   string
	ParallelBroadcast bool                  // fan-out reads in parallel during degraded mode
	Encryptor         *encryption.Encryptor // nil when encryption is disabled
	CounterBackend    CounterBackend        // nil uses LocalCounterBackend
}

// BackendManager manages multiple storage backends with quota tracking.
// Embeds *backendCore for shared infrastructure (backend map, store, usage,
// timeouts, routing) and adds the S3 API surface, encryption, caching,
// dashboard, and hot-reloadable configuration.
type BackendManager struct {
	*backendCore
	Rebalancer        *Rebalancer                     // periodic object distribution
	Replicator        *Replicator                     // background replica creation
	CleanupWorker     *CleanupWorker                  // retry queue for failed deletions
	DrainManager      *DrainManager                   // backend drain and remove operations
	MultipartManager  *MultipartManager               // multipart upload lifecycle
	ObjectManager     *ObjectManager                  // CRUD, read failover, broadcast reads
	dashboard         *DashboardAggregator            // web UI data aggregation
	usageFlushCfg     atomic.Pointer[config.UsageFlushConfig]
	lifecycleCfg      atomic.Pointer[config.LifecycleConfig]
}

// NewBackendManager creates a new backend manager with the given configuration.
func NewBackendManager(cfg *BackendManagerConfig) *BackendManager {
	backendNames := make([]string, 0, len(cfg.Backends))
	for name := range cfg.Backends {
		backendNames = append(backendNames, name)
	}

	counters := cfg.CounterBackend
	if counters == nil {
		counters = NewLocalCounterBackend(backendNames)
	}
	usage := NewUsageTracker(counters, cfg.UsageLimits)

	core := &backendCore{
		backends:        cfg.Backends,
		store:           cfg.Store,
		order:           cfg.Order,
		backendTimeout:  cfg.BackendTimeout,
		usage:           usage,
		routingStrategy: cfg.RoutingStrategy,
	}

	cleanupWorker := NewCleanupWorker(core)
	multipartManager := NewMultipartManager(core, cfg.Encryptor)
	cache := NewLocationCache(cfg.CacheTTL)
	objectManager := NewObjectManager(core, cfg.Encryptor, cache, cfg.ParallelBroadcast)

	m := &BackendManager{
		backendCore:      core,
		Rebalancer:       NewRebalancer(core),
		Replicator:       NewReplicator(core),
		CleanupWorker:    cleanupWorker,
		MultipartManager: multipartManager,
		ObjectManager:    objectManager,
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
func (m *BackendManager) UpdateUsageLimits(limits map[string]UsageLimits) {
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
	rb, ok := m.usage.backend.(*RedisCounterBackend)
	return ok && rb.IsHealthy()
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
	backend, err := m.getBackend(backendName)
	if err != nil {
		return 0, 0, err
	}

	// Unwrap any circuit breaker wrappers to get the concrete *S3Backend.
	inner := backend
	for {
		if u, ok := inner.(interface{ Unwrap() ObjectBackend }); ok {
			inner = u.Unwrap()
		} else {
			break
		}
	}
	s3b, ok := inner.(*S3Backend)
	if !ok {
		return 0, 0, fmt.Errorf("backend %s does not support listing", backendName)
	}

	slog.Info("Starting backend sync", "backend", backendName, "bucket", bucket)

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

	err = s3b.ListObjects(ctx, "", func(objects []ListedObject) error {
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

	slog.Info("Backend sync complete", "backend", backendName, "bucket", bucket,
		"imported", imported, "skipped", skipped)
	return imported, skipped, nil
}


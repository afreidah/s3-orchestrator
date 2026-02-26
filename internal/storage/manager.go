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
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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
	ParallelBroadcast bool // fan-out reads in parallel during degraded mode
}

// BackendManager manages multiple storage backends with quota tracking.
type BackendManager struct {
	backends       map[string]ObjectBackend      // name -> backend
	store          MetadataStore                 // metadata persistence (Store or CircuitBreakerStore)
	order          []string                      // backend selection order
	cache          *LocationCache                // key -> cached backend (for degraded reads)
	backendTimeout time.Duration                 // per-operation timeout for backend S3 calls
	usage          *UsageTracker                 // per-backend usage counters and limits
	metrics        *MetricsCollector             // Prometheus metric recording and gauge refresh
	dashboard      *DashboardAggregator          // web UI data aggregation
	routingStrategy   string                        // "pack" or "spread"
	parallelBroadcast bool                          // fan-out reads in parallel during degraded mode
	rebalanceCfg      atomic.Pointer[config.RebalanceConfig]
	replicationCfg  atomic.Pointer[config.ReplicationConfig]
	usageFlushCfg   atomic.Pointer[config.UsageFlushConfig]
	lifecycleCfg    atomic.Pointer[config.LifecycleConfig]
}

// NewBackendManager creates a new backend manager with the given configuration.
func NewBackendManager(cfg *BackendManagerConfig) *BackendManager {
	backendNames := make([]string, 0, len(cfg.Backends))
	for name := range cfg.Backends {
		backendNames = append(backendNames, name)
	}

	usage := NewUsageTracker(backendNames, cfg.UsageLimits)

	return &BackendManager{
		backends:        cfg.Backends,
		store:           cfg.Store,
		order:           cfg.Order,
		cache:           NewLocationCache(cfg.CacheTTL),
		backendTimeout:  cfg.BackendTimeout,
		usage:           usage,
		metrics:         NewMetricsCollector(cfg.Store, usage, backendNames),
		dashboard:       NewDashboardAggregator(cfg.Store, usage, cfg.Order),
		routingStrategy:   cfg.RoutingStrategy,
		parallelBroadcast: cfg.ParallelBroadcast,
	}
}

// GetParts returns all parts for a multipart upload. Delegates to the metadata
// store, keeping the store behind the interface.
func (m *BackendManager) GetParts(ctx context.Context, uploadID string) ([]MultipartPart, error) {
	return m.store.GetParts(ctx, uploadID)
}

// ClearCache removes all entries from the location cache.
func (m *BackendManager) ClearCache() {
	m.cache.Clear()
}

// Close stops the background cache eviction goroutine. Safe to call multiple times.
func (m *BackendManager) Close() {
	m.cache.Close()
}

// UpdateUsageLimits replaces the per-backend usage limits. Safe to call
// concurrently with request handling.
func (m *BackendManager) UpdateUsageLimits(limits map[string]UsageLimits) {
	m.usage.UpdateLimits(limits)
}

// FlushUsage flushes accumulated in-memory usage counters to the database.
func (m *BackendManager) FlushUsage(ctx context.Context) error {
	return m.usage.FlushUsage(ctx, m.store)
}

// SetRebalanceConfig atomically stores the rebalance configuration.
func (m *BackendManager) SetRebalanceConfig(cfg *config.RebalanceConfig) {
	m.rebalanceCfg.Store(cfg)
}

// RebalanceConfig returns the current rebalance configuration.
func (m *BackendManager) RebalanceConfig() *config.RebalanceConfig {
	return m.rebalanceCfg.Load()
}

// SetReplicationConfig atomically stores the replication configuration.
func (m *BackendManager) SetReplicationConfig(cfg *config.ReplicationConfig) {
	m.replicationCfg.Store(cfg)
}

// ReplicationConfig returns the current replication configuration.
func (m *BackendManager) ReplicationConfig() *config.ReplicationConfig {
	return m.replicationCfg.Load()
}

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

// classifyWriteError translates store errors from write-path operations into
// S3-compatible errors and updates the tracing span. Handles the three common
// cases: database unavailable (503), no space available (507), and generic
// errors. Returns the translated error.
func (m *BackendManager) classifyWriteError(span trace.Span, operation string, err error) error {
	if errors.Is(err, ErrDBUnavailable) {
		span.SetStatus(codes.Error, "database unavailable")
		telemetry.DegradedWriteRejectionsTotal.WithLabelValues(operation).Inc()
		return ErrServiceUnavailable
	}
	if errors.Is(err, ErrNoSpaceAvailable) {
		span.SetStatus(codes.Error, "insufficient storage")
		return ErrInsufficientStorage
	}
	span.SetStatus(codes.Error, err.Error())
	span.RecordError(err)
	return err
}

// recordObjectOrCleanup calls RecordObject and, on failure, deletes the orphaned
// object from the backend. Updates the tracing span on error.
func (m *BackendManager) recordObjectOrCleanup(ctx context.Context, span trace.Span, backend ObjectBackend, key, backendName string, size int64) error {
	if err := m.store.RecordObject(ctx, key, backendName, size); err != nil {
		slog.Error("RecordObject failed, cleaning up orphan",
			"key", key, "backend", backendName, "error", err)
		if delErr := backend.DeleteObject(ctx, key); delErr != nil {
			slog.Error("Failed to clean up orphaned object",
				"key", key, "backend", backendName, "error", delErr)
			m.enqueueCleanup(ctx, backendName, key, "orphan_record_failed")
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return fmt.Errorf("failed to record object: %w", err)
	}
	return nil
}

// getBackend returns the named backend, or an error if it doesn't exist.
func (m *BackendManager) getBackend(name string) (ObjectBackend, error) {
	b, ok := m.backends[name]
	if !ok {
		return nil, fmt.Errorf("backend %s not found", name)
	}
	return b, nil
}

// withTimeout returns a context with the configured backend timeout applied.
// If no timeout is configured, the original context is returned unchanged.
func (m *BackendManager) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.backendTimeout > 0 {
		return context.WithTimeout(ctx, m.backendTimeout)
	}
	return ctx, func() {}
}

// GenerateUploadID creates a random hex string for multipart upload IDs.
func GenerateUploadID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// selectBackendForWrite picks the target backend for a write operation using
// the configured routing strategy. "pack" returns the first backend with space,
// "spread" returns the least-utilized backend.
func (m *BackendManager) selectBackendForWrite(ctx context.Context, size int64, eligible []string) (string, error) {
	if m.routingStrategy == "spread" {
		return m.store.GetLeastUtilizedBackend(ctx, size, eligible)
	}
	return m.store.GetBackendWithSpace(ctx, size, eligible)
}

// recordOperation delegates to the MetricsCollector.
func (m *BackendManager) recordOperation(operation, backend string, start time.Time, err error) {
	m.metrics.RecordOperation(operation, backend, start, err)
}

// UpdateQuotaMetrics refreshes Prometheus gauges from the metadata store.
func (m *BackendManager) UpdateQuotaMetrics(ctx context.Context) error {
	return m.metrics.UpdateQuotaMetrics(ctx)
}

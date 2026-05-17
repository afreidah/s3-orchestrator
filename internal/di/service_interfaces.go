// -------------------------------------------------------------------------------
// Service Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow interfaces listing only the subset of *proxy.BackendManager
// (or its sub-managers) that each lifecycle.Runner service in
// services.go actually calls. Each service constructor takes the
// matching interface; the concrete *proxy.BackendManager and
// *multipart.Manager satisfy them implicitly, so the DI wiring in
// services.go passes the resolved concrete value through without
// requiring per-interface providers.
//
// Mirrors the consumer-declared-interfaces pattern documented in
// docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// usageFlushOps is the subset of *proxy.BackendManager that
// usageFlushService needs to read flush configuration, decide whether
// to acquire an advisory lock, and run the flush + metrics tick.
type usageFlushOps interface {
	UsageFlushConfig() *config.UsageFlushConfig
	NearUsageLimit(threshold float64) bool
	RedisCounterConfigured() bool
	FlushUsage(ctx context.Context) error
	UpdateQuotaMetrics(ctx context.Context) error
}

// staleMultipartCleaner is the subset of *multipart.Manager that
// NewMultipartCleanupService needs to drive the stale-upload sweep.
type staleMultipartCleaner interface {
	CleanupStaleMultipartUploads(ctx context.Context, olderThan time.Duration)
}

// lifecycleOps is the subset of *proxy.BackendManager that
// NewLifecycleService needs to read the lifecycle config and process a
// tick.
type lifecycleOps interface {
	LifecycleConfig() *config.LifecycleConfig
	ProcessLifecycleRules(ctx context.Context, rules []config.LifecycleRule) (deleted, failed int)
}

// quotaMetricsRefresher is the single-method subset of
// *proxy.BackendManager that the rebalance / over-replication /
// replicator pass-result helpers call to push fresh quota gauges after a
// successful pass. Shared by handlePassResult so the three constructors
// agree on the dependency surface.
type quotaMetricsRefresher interface {
	UpdateQuotaMetrics(ctx context.Context) error
}

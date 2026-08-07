// -------------------------------------------------------------------------------
// Service Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow contracts each lifecycle.Runner service in services.go pulls
// from *proxy.BackendManager (or its sub-managers). DI wiring still
// passes the concrete value through without per-interface providers.
// Pattern rationale: docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package di

import (
	"context"

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

// lifecycleOps is the subset of *expiry.Manager that NewLifecycleService
// needs to read the lifecycle config and process a tick.
type lifecycleOps interface {
	Config() *config.LifecycleConfig
	ProcessRules(ctx context.Context, rules []config.LifecycleRule) (deleted, failed int)
}

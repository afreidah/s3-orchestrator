// -------------------------------------------------------------------------------
// Service Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow contracts each lifecycle.Runner service in services.go pulls from
// the collaborators DI builds. Each names one provider's surface, so a
// service's dependency footprint is readable without opening the provider.
// Pattern rationale: docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package di

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/progress"
)

// usageFlushOps is the subset of *usage.Service that usageFlushService needs
// to read flush configuration, decide whether to acquire an advisory lock,
// and run the flush itself.
type usageFlushOps interface {
	Config() *config.UsageFlushConfig
	RedisCounterConfigured() bool
	FlushUsage(ctx context.Context) error
}

// nearLimitReporter is the *counter.UsageTracker read that drives the
// adaptive tick. Declared against the tracker rather than routed through a
// service, because a backend approaching its limit is a fact about the
// counters and nothing in between adds to it.
type nearLimitReporter interface {
	NearLimit(threshold float64) bool
}

// quotaMetricsRefresher is the *infra.BackendRuntime call that republishes the
// quota gauges after a flush, so the numbers operators watch move with the
// ones just written.
type quotaMetricsRefresher interface {
	UpdateQuotaMetrics(ctx context.Context) error
}

// lifecycleOps is the subset of *expiry.Manager that NewLifecycleService
// needs to read the lifecycle config and process a tick.
type lifecycleOps interface {
	Config() *config.LifecycleConfig
	ProcessRules(ctx context.Context, rules []config.LifecycleRule, observer progress.Observer) (deleted, failed int)
}

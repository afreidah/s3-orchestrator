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
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// usageFlushOps is the subset of *usage.Service that usageFlushService needs
// to decide whether to load the shared fleet state and run the flush itself.
type usageFlushOps interface {
	RedisCounterConfigured() bool
	FlushUsage(ctx context.Context) error
	RefreshQuotaBaselines(ctx context.Context) (map[string]core.QuotaStat, error)
}

// nearLimitReporter is the *counter.UsageTracker read that drives the
// adaptive tick. Declared against the tracker rather than routed through a
// service, because a backend approaching its limit is a fact about the
// counters and nothing in between adds to it.
type nearLimitReporter interface {
	NearLimit(threshold float64) bool
}

// gaugeRefresher is the *metrics.Collector set the flush tick runs after
// writing counters: every instance loads the published worker gauges and
// republishes the gauges the store backs.
type gaugeRefresher interface {
	LoadWorkerGauges(ctx context.Context) error
	RefreshGauges(ctx context.Context, stats map[string]core.QuotaStat) error
}

// backendStateRefresher reloads this instance's cached backend states from the
// records. *backendstate.Manager satisfies it.
type backendStateRefresher interface {
	Refresh(ctx context.Context) error
}

// lifecycleOps is the subset of *expiry.Manager that NewLifecycleService
// needs to read the lifecycle config and process a tick.
type lifecycleOps interface {
	Config() *config.LifecycleConfig
	ProcessRules(ctx context.Context, rules []config.LifecycleRule, observer progress.Observer) batch.Summary
}

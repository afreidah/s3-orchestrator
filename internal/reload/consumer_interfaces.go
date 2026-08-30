// -------------------------------------------------------------------------------
// Reload Hook Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow contracts each reload hook in hooks.go pulls from the collaborator
// that owns the section being swapped. DI still returns the concrete value;
// the local interface restricts what the hook can call. Pattern rationale:
// docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package reload

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// usageLimitsApplier is the *counter.UsageTracker call the usage-limits
// reload hook needs. The limits belong to the counters, so the hook writes
// them there rather than through anything holding the counters.
type usageLimitsApplier interface {
	UpdateLimits(limits map[string]core.UsageLimits)
}

// usageFlushConfigApplier swaps the usage-flush section on the *usage.Service
// that reads it.
type usageFlushConfigApplier interface {
	SetConfig(cfg *config.UsageFlushConfig)
}

// quotaMetricsRefresher republishes the quota gauges after a swap, so what
// operators watch reflects the configuration now in force.
type quotaMetricsRefresher interface {
	UpdateQuotaMetrics(ctx context.Context) error
}

// lifecycleConfigApplier is the reloadable surface of *expiry.Manager, which
// owns the lifecycle rules it applies.
type lifecycleConfigApplier interface {
	SetConfig(cfg *config.LifecycleConfig)
}

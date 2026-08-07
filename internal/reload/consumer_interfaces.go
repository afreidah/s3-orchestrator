// -------------------------------------------------------------------------------
// Reload Hook Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow contracts each reload hook in hooks.go pulls from
// *proxy.BackendManager. DI still returns the concrete value; the local
// interface restricts what the hook can call. Pattern rationale:
// docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package reload

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// usageLimitsApplier is the subset of *proxy.BackendManager the
// usage-limits reload hook needs.
type usageLimitsApplier interface {
	UpdateUsageLimits(limits map[string]core.UsageLimits)
}

// managerConfigApplier is the subset of *proxy.BackendManager the
// manager-config reload hook needs to swap UsageFlush and Integrity sections
// and refresh quota gauges.
type managerConfigApplier interface {
	SetUsageFlushConfig(cfg *config.UsageFlushConfig)
	SetIntegrityConfig(cfg *config.IntegrityConfig)
	UpdateQuotaMetrics(ctx context.Context) error
}

// lifecycleConfigApplier is the reloadable surface of *expiry.Manager, which
// owns the lifecycle rules it applies.
type lifecycleConfigApplier interface {
	SetConfig(cfg *config.LifecycleConfig)
}

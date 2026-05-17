// -------------------------------------------------------------------------------
// Reload Hook Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow interfaces listing only the subset of *proxy.BackendManager
// each reload hook in hooks.go actually calls. The DI resolution still
// returns the concrete *proxy.BackendManager (which satisfies these
// interfaces implicitly); the local variable typing in each hook
// documents and restricts what the hook is allowed to call.
//
// Mirrors the consumer-declared-interfaces pattern documented in
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
// manager-config reload hook needs to swap UsageFlush, Lifecycle, and
// Integrity sections and refresh quota gauges.
type managerConfigApplier interface {
	SetUsageFlushConfig(cfg *config.UsageFlushConfig)
	SetLifecycleConfig(cfg *config.LifecycleConfig)
	SetIntegrityConfig(cfg *config.IntegrityConfig)
	UpdateQuotaMetrics(ctx context.Context) error
}

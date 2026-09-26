// -------------------------------------------------------------------------------
// Circuit Breaker Watchdog - Background Service
//
// Author: Alex Freidah
//
// Probes every breaker registered in the breaker.Registry on a short tick.
// Each breaker decides what a probe means for it: resetting a stale half-open
// probe, or health-checking its dependency while open. Membership in the
// registry is decided once at DI construction time, so the watchdog itself
// contains no type-assertion or backend-discovery logic.
// -------------------------------------------------------------------------------

package breaker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
)

// DefaultWatchdogInterval is the cadence at which the watchdog probes every
// registered breaker. It bounds how late a due health check can run, so it
// stays well below the shortest sensible open timeout.
const DefaultWatchdogInterval = 5 * time.Second

// watchdog is the lifecycle.Runner that probes every registered breaker on a
// tick.
type watchdog struct {
	registry *Registry
	interval time.Duration
}

// NewWatchdog constructs the watchdog background service. The registry
// holds every breaker that should be probed on a tick - membership is
// decided once at DI construction time.
func NewWatchdog(registry *Registry) lifecycle.Runner {
	return &watchdog{registry: registry, interval: DefaultWatchdogInterval}
}

// Run implements lifecycle.Runner. Probes every breaker each interval until
// ctx is cancelled.
func (w *watchdog) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.registry.ProbeAll(ctx)
		}
	}
}

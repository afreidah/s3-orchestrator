// -------------------------------------------------------------------------------
// Circuit Breaker Watchdog - Background Service
//
// Author: Alex Freidah
//
// Periodically resets stale half-open probes on every breaker
// registered in the breaker.Registry, preventing circuits from getting
// stuck half-open indefinitely when no new requests arrive.
// Membership in the registry is decided once at DI construction time,
// so the watchdog itself contains no type-assertion or
// backend-discovery logic. Lives in the breaker package (#925).
// -------------------------------------------------------------------------------

package breaker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
)

// DefaultWatchdogInterval is the cadence at which the watchdog
// inspects every registered breaker. Picked as half the breaker probe
// timeout so a stuck half-open state is detected within one full probe
// window.
const DefaultWatchdogInterval = 1 * time.Minute

// watchdog is the lifecycle.Runner that scans every registered
// breaker on a tick.
type watchdog struct {
	registry *Registry
}

// NewWatchdog constructs the watchdog background service. The registry
// holds every breaker that should be inspected on a tick - membership
// is decided once at DI construction time.
func NewWatchdog(registry *Registry) lifecycle.Runner {
	return &watchdog{registry: registry}
}

// Run implements lifecycle.Runner. Checks every DefaultWatchdogInterval
// (1 minute) - half the breaker probe timeout.
func (w *watchdog) Run(ctx context.Context) error {
	ticker := time.NewTicker(DefaultWatchdogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.checkAll()
		}
	}
}

// checkAll resets stale probes on every registered breaker.
func (w *watchdog) checkAll() {
	w.registry.ResetStaleProbes()
}

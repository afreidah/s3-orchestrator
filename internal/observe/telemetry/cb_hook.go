// -------------------------------------------------------------------------------
// Circuit Breaker Observability Hook
//
// Author: Alex Freidah
//
// Bridges the breaker package's transition callback to Prometheus metrics
// and the notification event bus. The breaker package itself imports
// nothing observability-related; this file is the single wiring point.
// -------------------------------------------------------------------------------

package telemetry

import (
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
)

// NewCircuitBreakerHook returns the callback to install on a breaker so its
// state changes drive the CircuitBreakerState gauge, the
// CircuitBreakerTransitionsTotal counter, and the BackendCircuitOpened /
// BackendCircuitClosed events.
//
// Initializes the gauge to "closed" up front so Prometheus reports a value
// before the first transition.
func NewCircuitBreakerHook(name string) func(breaker.StateChangeInfo) {
	CircuitBreakerState.WithLabelValues(name).Set(float64(breaker.StateClosed))

	return func(info breaker.StateChangeInfo) {
		CircuitBreakerState.WithLabelValues(info.Name).Set(float64(info.To))
		CircuitBreakerTransitionsTotal.WithLabelValues(info.Name, info.From.String(), info.To.String()).Inc()
		emitCircuitEvent(info)
	}
}

// NewDatabaseBreakerHook chains the standard breaker hook with the
// DegradedModeActive gauge (1 when not closed, 0 when closed).
func NewDatabaseBreakerHook(name string) func(breaker.StateChangeInfo) {
	DegradedModeActive.Set(0)
	standard := NewCircuitBreakerHook(name)
	return func(info breaker.StateChangeInfo) {
		standard(info)
		if info.To == breaker.StateClosed {
			DegradedModeActive.Set(0)
		} else {
			DegradedModeActive.Set(1)
		}
	}
}

// emitCircuitEvent sends the corresponding BackendCircuit{Opened,Closed}
// event for the closed->open and *->closed transitions. Other transitions
// are state-machine internals and not user-visible.
func emitCircuitEvent(info breaker.StateChangeInfo) {
	if event.Emit == nil {
		return
	}
	switch {
	case info.To == breaker.StateOpen && info.From == breaker.StateClosed:
		event.Emit(event.Event{
			Type:    event.BackendCircuitOpened,
			Subject: info.Name,
			Data: map[string]any{
				"backend":   info.Name,
				"failures":  info.Failures,
				"threshold": info.Threshold,
			},
		})
	case info.To == breaker.StateClosed:
		event.Emit(event.Event{
			Type:    event.BackendCircuitClosed,
			Subject: info.Name,
			Data: map[string]any{
				"backend":           info.Name,
				"degraded_duration": info.OpenDuration.Round(time.Millisecond).String(),
			},
		})
	}
}
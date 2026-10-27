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
	"maps"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
)

// NewCircuitBreakerHook returns the callback to install on a breaker so its
// state changes drive the CircuitBreakerState gauge, the
// CircuitBreakerTransitionsTotal counter, and the BackendCircuitOpened /
// BackendCircuitClosed events. It sets the gauge to closed up front so a value
// is reported before the first transition.
func NewCircuitBreakerHook(name string) func(breaker.StateChangeInfo) {
	record := NewBackendCircuitHook(name)
	return func(info breaker.StateChangeInfo) {
		record(info)
		emitCircuitEvent(info, info.Name, nil)
	}
}

// NewBackendCircuitHook returns the callback for a backend's aggregate state:
// it drives the gauge and counter as NewCircuitBreakerHook does but publishes
// no event, since each of the backend's breakers publishes its own through
// BackendCircuitEvent.
func NewBackendCircuitHook(name string) func(breaker.StateChangeInfo) {
	CircuitBreakerState.WithLabelValues(name).Set(float64(breaker.StateClosed))

	return func(info breaker.StateChangeInfo) {
		CircuitBreakerState.WithLabelValues(info.Name).Set(float64(info.To))
		CircuitBreakerTransitionsTotal.WithLabelValues(info.Name, info.From.String(), info.To.String()).Inc()
	}
}

// BackendCircuitEvent publishes the open or close of one of a backend's
// breakers, naming the call it guards, or "all" for the backend breaker.
func BackendCircuitEvent(backend, call string, info breaker.StateChangeInfo) {
	emitCircuitEvent(info, backend, map[string]any{"call": call})
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

// emitCircuitEvent sends the BackendCircuitOpened or BackendCircuitClosed
// event for a transition, about subject and carrying extra.
func emitCircuitEvent(info breaker.StateChangeInfo, subject string, extra map[string]any) {
	typ := event.BackendCircuitClosed
	data := map[string]any{
		"backend":           subject,
		"degraded_duration": info.OpenDuration.Round(time.Millisecond).String(),
	}
	if info.To == breaker.StateOpen {
		typ = event.BackendCircuitOpened
		data = map[string]any{
			"backend":   subject,
			"failures":  info.Failures,
			"threshold": info.Threshold,
		}
	}
	maps.Copy(data, extra)
	event.Publish(typ, subject, data)
}

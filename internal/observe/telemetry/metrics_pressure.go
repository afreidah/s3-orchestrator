// -------------------------------------------------------------------------------
// Backend Pressure Metrics
//
// Author: Alex Freidah
//
// The shares of each backend's calls that were slow or hit the backend
// timeout, the reasons a backend is under pressure, and the baseline its calls
// are judged against, plus the event a change in pressure publishes. The
// pressure tracker sets them on every evaluation tick.
// -------------------------------------------------------------------------------

package telemetry

import "github.com/afreidah/s3-orchestrator/internal/observe/event"

var (
	// BackendPressure is the share of a backend's calls in a window that were
	// slow (signal="slow") or ended by the backend timeout
	// (signal="timeouts"). The 60s window decides pressure.
	BackendPressure = gaugeVec("s3o_backend_pressure",
		"Share of a backend's calls that were slow or timed out, per window", "backend", "signal", "window")

	// BackendPressured is 1 for each reason a backend is under pressure, which
	// sorts it last among a read's copies and a write's targets.
	BackendPressured = gaugeVec("s3o_backend_pressured",
		"Reasons a backend is under pressure, 1 per reason held", "backend", "reason")

	// BackendLatencyBaseline is a backend's typical call time, the baseline a
	// call is judged slow against.
	BackendLatencyBaseline = gaugeVec("s3o_backend_latency_baseline_seconds",
		"Typical call time a backend's calls are judged slow against", "backend")
)

// BackendPressureEvent publishes a backend coming under pressure, with the
// reasons it holds, or leaving it when reasons is empty, with the deciding
// window's shares either way.
func BackendPressureEvent(backend string, reasons []string, slowShare, timeoutShare float64) {
	typ := event.BackendPressureRestored
	if len(reasons) > 0 {
		typ = event.BackendPressureDemoted
	}
	event.Publish(typ, backend, map[string]any{
		"backend":       backend,
		"reasons":       reasons,
		"slow_share":    slowShare,
		"timeout_share": timeoutShare,
	})
}

// -------------------------------------------------------------------------------
// Worker Health Metrics
//
// Author: Alex Freidah
//
// Per-service tick counters and timestamps for the locked-ticker
// background workers. Operators alert on these to catch stalled or
// repeatedly failing workers without scraping logs:
//
//   - rate(worker_ticks_total{result="error"}[15m]) > 0  -> persistent failures
//   - time() - worker_last_success_timestamp_seconds > N -> staleness
//   - worker_consecutive_failures > N                    -> degraded but running
// -------------------------------------------------------------------------------

package telemetry

import "github.com/prometheus/client_golang/prometheus"

// WorkerTicksTotal counts each completed tick of a locked-ticker service,
// labelled by service name and outcome: success when the work returned nil,
// error when it returned one, and skipped when shouldRun gated the tick or the
// advisory lock was busy.
var (
	WorkerTicksTotal = counterVec("s3o_worker_ticks_total",
		"Locked-ticker service ticks by service and outcome", "service", "result")

	// WorkerLastSuccessTimestampSeconds records the Unix time of the
	// most recent successful tick for each service. Alert on staleness
	// with time() minus this gauge.
	WorkerLastSuccessTimestampSeconds = gaugeVec("s3o_worker_last_success_timestamp_seconds",
		"Unix time of the most recent successful tick per service", "service")

	// WorkerConsecutiveFailures gauges the run of back-to-back failed
	// ticks for each service. Resets to 0 on the next success.
	WorkerConsecutiveFailures = gaugeVec("s3o_worker_consecutive_failures",
		"Consecutive failed ticks per service since the last success", "service")

	// WorkerCyclesTotal counts each worker pass by how its items ended:
	// success, partial, failed, empty, or error when the pass failed before it
	// had a tally. A tick reports whether the pass ran; this reports what it did.
	WorkerCyclesTotal = counterVec("s3o_worker_cycles_total",
		"Worker passes by service and outcome of their items", "service", "outcome")

	// WorkerCycleDurationSeconds times each worker pass, whatever its outcome.
	WorkerCycleDurationSeconds = histogramVec("s3o_worker_cycle_duration_seconds",
		"Worker pass duration by service", prometheus.ExponentialBuckets(0.1, 2, 14), "service")
)

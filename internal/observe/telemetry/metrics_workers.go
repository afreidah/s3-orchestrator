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

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// WorkerTicksTotal counts each completed tick of a locked-ticker service,
// labelled by service name and outcome: success when the work returned nil,
// error when it returned one, and skipped when shouldRun gated the tick or the
// advisory lock was busy.
var (
	WorkerTicksTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_worker_ticks_total",
			Help: "Locked-ticker service ticks by service and outcome",
		},
		[]string{"service", "result"},
	)

	// WorkerLastSuccessTimestampSeconds records the Unix time of the
	// most recent successful tick for each service. Alert on staleness
	// with time() minus this gauge.
	WorkerLastSuccessTimestampSeconds = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_worker_last_success_timestamp_seconds",
			Help: "Unix time of the most recent successful tick per service",
		},
		[]string{"service"},
	)

	// WorkerConsecutiveFailures gauges the run of back-to-back failed
	// ticks for each service. Resets to 0 on the next success.
	WorkerConsecutiveFailures = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_worker_consecutive_failures",
			Help: "Consecutive failed ticks per service since the last success",
		},
		[]string{"service"},
	)
)

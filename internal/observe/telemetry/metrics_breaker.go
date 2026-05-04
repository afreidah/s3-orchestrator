// -------------------------------------------------------------------------------
// Metrics  -  Circuit Breaker
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface. Split out of the
// original 784-line metrics.go to keep each subsystem under ~150 lines.
// -------------------------------------------------------------------------------

package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// CircuitBreakerState and related package-level variables used by this package.
var (
	// --- Circuit breaker metrics ---

	// CircuitBreakerState tracks the current circuit breaker state per component.
	// 0=closed (healthy), 1=open (down), 2=half-open (probing).
	CircuitBreakerState = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_circuit_breaker_state",
			Help: "Current circuit breaker state: 0=closed, 1=open, 2=half-open",
		},
		[]string{"name"},
	)

	// CircuitBreakerTransitionsTotal counts state transitions per component.
	CircuitBreakerTransitionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_circuit_breaker_transitions_total",
			Help: "Total number of circuit breaker state transitions",
		},
		[]string{"name", "from", "to"},
	)

	// CircuitBreakerInternalErrorsTotal counts errors returned by the
	// breaker's own machinery (PostCheck / state transition helpers).
	// Non-zero values indicate a bookkeeping bug, not an application
	// error  -  alert on any increase.
	CircuitBreakerInternalErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_circuit_breaker_internal_errors_total",
			Help: "Errors returned by circuit breaker PostCheck/state transitions",
		},
		[]string{"name", "operation"},
	)

	// DegradedReadsTotal counts reads served via broadcast during degraded mode.
	DegradedReadsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_degraded_reads_total",
			Help: "Total number of read operations served via broadcast during degraded mode",
		},
		[]string{"operation"},
	)

	// DegradedCacheHitsTotal counts location cache hits during degraded reads.
	DegradedCacheHitsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_degraded_cache_hits_total",
			Help: "Total number of location cache hits during degraded reads",
		},
	)

	// DegradedWriteRejectionsTotal counts writes rejected during degraded mode.
	DegradedWriteRejectionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_degraded_write_rejections_total",
			Help: "Total number of write operations rejected during degraded mode",
		},
		[]string{"operation"},
	)

	// WriteFailoverTotal counts writes that failed on one backend and were
	// retried on another. Labels: operation, failed_backend, success_backend.
	WriteFailoverTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_write_failover_total",
			Help: "Total number of write operations that failed over to a different backend",
		},
		[]string{"operation", "failed_backend", "success_backend"},
	)

)

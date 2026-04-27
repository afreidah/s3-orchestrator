// -------------------------------------------------------------------------------
// Metrics — Request, Backend, Manager, Rate Limit
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

var (
	// --- Request metrics ---

	// RequestsTotal counts all HTTP requests by method and status code.
	RequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_requests_total",
			Help: "Total number of HTTP requests processed",
		},
		[]string{"method", "status_code"},
	)

	// RequestDuration tracks request latency distribution by method.
	RequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3o_request_duration_seconds",
			Help:    "HTTP request latency in seconds",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
		},
		[]string{"method"},
	)

	// RequestSize tracks upload sizes.
	RequestSize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3o_request_size_bytes",
			Help:    "HTTP request body size in bytes",
			Buckets: prometheus.ExponentialBuckets(1024, 4, 10), // 1KB to 256GB
		},
		[]string{"method"},
	)

	// ResponseSize tracks download sizes.
	ResponseSize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3o_response_size_bytes",
			Help:    "HTTP response body size in bytes",
			Buckets: prometheus.ExponentialBuckets(1024, 4, 10), // 1KB to 256GB
		},
		[]string{"method"},
	)

	// InflightRequests tracks currently processing requests.
	InflightRequests = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_inflight_requests",
			Help: "Number of requests currently being processed",
		},
		[]string{"method"},
	)

	// --- Backend metrics ---

	// BackendRequestsTotal counts backend operations by operation type and status.
	BackendRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_backend_requests_total",
			Help: "Total number of backend storage operations",
		},
		[]string{"operation", "backend", "status"},
	)

	// BackendDuration tracks backend operation latency.
	BackendDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3o_backend_duration_seconds",
			Help:    "Backend operation latency in seconds",
			Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120},
		},
		[]string{"operation", "backend"},
	)

	// --- Manager metrics ---

	// ManagerRequestsTotal counts manager-level operations.
	ManagerRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_manager_requests_total",
			Help: "Total number of manager-level storage operations",
		},
		[]string{"operation", "backend", "status"},
	)

	// ManagerDuration tracks manager operation latency.
	ManagerDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3o_manager_duration_seconds",
			Help:    "Manager operation latency in seconds",
			Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120},
		},
		[]string{"operation", "backend"},
	)

	// --- Rate limit metrics ---

	// RateLimitRejectionsTotal counts requests rejected by the per-IP rate limiter.
	RateLimitRejectionsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_rate_limit_rejections_total",
			Help: "Total requests rejected due to per-IP rate limiting",
		},
	)

	// AdmissionRejectionsTotal counts requests rejected by server-level admission control.
	AdmissionRejectionsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_admission_rejections_total",
			Help: "Total requests rejected due to server-level admission control",
		},
	)

	// WorkerAdmissionRejectionsTotal counts background worker tasks that were
	// skipped because the admission semaphore was full.
	WorkerAdmissionRejectionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_worker_admission_rejections_total",
			Help: "Background worker tasks skipped due to admission control",
		},
		[]string{"worker"},
	)

	// LoadShedTotal counts requests probabilistically rejected by active
	// load shedding before reaching the hard admission limit.
	LoadShedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_load_shed_total",
			Help: "Requests shed by active load shedding before hard admission limit",
		},
	)

	// EarlyRejectionsTotal counts uploads rejected before body transmission
	// via Expect: 100-Continue pre-flight capacity checks.
	EarlyRejectionsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_early_rejections_total",
			Help: "Uploads rejected before body transmission (no backend capacity)",
		},
	)

)

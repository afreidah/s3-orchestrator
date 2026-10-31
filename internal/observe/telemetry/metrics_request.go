// -------------------------------------------------------------------------------
// Metrics  -  Request, Backend, Manager, Rate Limit
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface. Split out of the
// original 784-line metrics.go to keep each subsystem under ~150 lines.
// -------------------------------------------------------------------------------

package telemetry

import "github.com/prometheus/client_golang/prometheus"

// HTTP request count, latency and in-flight metrics.
var (
	// RequestsTotal counts all HTTP requests by method and status code.
	RequestsTotal = counterVec("s3o_requests_total", "Total number of HTTP requests processed", "method", "status_code")

	// RequestDuration tracks request latency distribution by method.
	RequestDuration = histogramVec("s3o_request_duration_seconds", "HTTP request latency in seconds",
		[]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}, "method")

	// RequestSize tracks upload sizes.
	RequestSize = histogramVec("s3o_request_size_bytes", "HTTP request body size in bytes",
		prometheus.ExponentialBuckets(1024, 4, 10), "method") // 1KB to 256GB

	// ResponseSize tracks download sizes.
	ResponseSize = histogramVec("s3o_response_size_bytes", "HTTP response body size in bytes",
		prometheus.ExponentialBuckets(1024, 4, 10), "method") // 1KB to 256GB

	// InflightRequests tracks currently processing requests.
	InflightRequests = gaugeVec("s3o_inflight_requests", "Number of requests currently being processed", "method")

	// BackendRequestsTotal counts backend operations by operation type and status.
	BackendRequestsTotal = counterVec("s3o_backend_requests_total",
		"Total number of backend storage operations", "operation", "backend", "status")

	// BackendDuration tracks backend operation latency.
	BackendDuration = histogramVec("s3o_backend_duration_seconds", "Backend operation latency in seconds",
		[]float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120}, "operation", "backend")

	// BackendRangeIgnoredTotal counts ranged GETs a backend answered with the
	// whole object, which the orchestrator then cut down to the range itself.
	BackendRangeIgnoredTotal = counterVec("s3o_backend_range_ignored_total",
		"Ranged GETs a backend answered with the whole object", "backend")

	// BackendUnsupported is 1 for each optional feature a backend has been found
	// not to support since the process started. Supported features have no series.
	BackendUnsupported = gaugeVec("s3o_backend_unsupported",
		"Optional features a backend refused, 1 per feature", "backend", "feature")

	// ManagerRequestsTotal counts manager-level operations.
	ManagerRequestsTotal = counterVec("s3o_manager_requests_total",
		"Total number of manager-level storage operations", "operation", "backend", "status")

	// ManagerDuration tracks manager operation latency.
	ManagerDuration = histogramVec("s3o_manager_duration_seconds", "Manager operation latency in seconds",
		[]float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120}, "operation", "backend")

	// RateLimitRejectionsTotal counts requests rejected by the per-IP rate limiter.
	RateLimitRejectionsTotal = counter("s3o_rate_limit_rejections_total",
		"Total requests rejected due to per-IP rate limiting")

	// AdmissionRejectionsTotal counts requests rejected by server-level admission control.
	AdmissionRejectionsTotal = counter("s3o_admission_rejections_total",
		"Total requests rejected due to server-level admission control")

	// AdmissionClientCanceledTotal counts requests where the client closed
	// or cancelled their context while waiting for an admission slot. These
	// are not server-side rejections and do not count against capacity SLOs.
	AdmissionClientCanceledTotal = counter("s3o_admission_client_canceled_total",
		"Total admission waits aborted by client context cancellation")

	// WorkerAdmissionRejectionsTotal counts background worker tasks that were
	// skipped because the admission semaphore was full.
	WorkerAdmissionRejectionsTotal = counterVec("s3o_worker_admission_rejections_total",
		"Background worker tasks skipped due to admission control", "worker")

	// LoadShedTotal counts requests probabilistically rejected by active
	// load shedding before reaching the hard admission limit.
	LoadShedTotal = counter("s3o_load_shed_total", "Requests shed by active load shedding before hard admission limit")

	// EarlyRejectionsTotal counts uploads rejected before body transmission
	// via Expect: 100-Continue pre-flight capacity checks.
	EarlyRejectionsTotal = counter("s3o_early_rejections_total",
		"Uploads rejected before body transmission (no backend capacity)")

	// HTTPPanicRecoveredTotal counts panics caught by the HTTP panic-
	// recovery middleware. A non-zero value means a request
	// handler panicked and the recovery layer translated it into a 500
	// response; the matching slog.ErrorContext line carries the panic
	// value, the captured stack, and the request id. Label scopes the
	// counter to the route group so dashboards can distinguish a
	// flaking S3 path from a flaking admin or UI path.
	HTTPPanicRecoveredTotal = counterVec("s3o_http_panic_recovered_total",
		"HTTP handler panics caught by the recovery middleware, by route group", "route")
)

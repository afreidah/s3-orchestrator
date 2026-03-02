// -------------------------------------------------------------------------------
// Admission Controller - Global Concurrent Request Limiter
//
// Author: Alex Freidah
//
// Server-level admission control using a channel-based semaphore. When the
// maximum number of concurrent requests is reached, new requests are rejected
// immediately with 503 SlowDown to prevent backend saturation and goroutine
// accumulation under load.
// -------------------------------------------------------------------------------

package server

import (
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// AdmissionController limits the number of concurrent in-flight requests.
type AdmissionController struct {
	sem chan struct{}
}

// NewAdmissionController creates an admission controller with the given
// concurrency limit. The limit must be positive.
func NewAdmissionController(maxConcurrent int) *AdmissionController {
	return &AdmissionController{
		sem: make(chan struct{}, maxConcurrent),
	}
}

// Middleware wraps an http.Handler with global admission control. Requests
// that exceed the concurrency limit receive 503 SlowDown immediately.
func (ac *AdmissionController) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case ac.sem <- struct{}{}:
			defer func() { <-ac.sem }()
			next.ServeHTTP(w, r)
		default:
			telemetry.AdmissionRejectionsTotal.Inc()
			writeS3Error(w, http.StatusServiceUnavailable, "SlowDown", "Server at capacity")
		}
	})
}

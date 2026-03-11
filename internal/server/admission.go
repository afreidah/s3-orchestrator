// -------------------------------------------------------------------------------
// Admission Controller - Concurrent Request Limiter with Read/Write Pools
//
// Author: Alex Freidah
//
// Server-level admission control using channel-based semaphores. Supports a
// single global pool or separate read/write pools. Optional active load
// shedding probabilistically rejects requests before the hard limit using a
// linear ramp from a configurable pressure threshold to full capacity. When
// the concurrency limit is reached, new requests are rejected with 503
// SlowDown and a Retry-After header.
// -------------------------------------------------------------------------------

package server

import (
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// AdmissionController limits the number of concurrent in-flight requests.
// When readSem and writeSem are set, reads and writes are tracked in
// separate pools; otherwise the global sem is used for all requests.
type AdmissionController struct {
	sem           chan struct{} // global pool (nil when split pools are used)
	readSem       chan struct{} // read pool (nil when global pool is used)
	writeSem      chan struct{} // write pool (nil when global pool is used)
	shedThreshold float64       // 0 = disabled, e.g. 0.8 = shed at 80%
	admissionWait time.Duration // 0 = instant reject (default)
}

// NewAdmissionController creates an admission controller with a single
// global concurrency limit. The limit must be positive.
func NewAdmissionController(maxConcurrent int) *AdmissionController {
	return &AdmissionController{
		sem: make(chan struct{}, maxConcurrent),
	}
}

// NewSplitAdmissionController creates an admission controller with separate
// concurrency limits for reads and writes. Both limits must be positive.
func NewSplitAdmissionController(maxReads, maxWrites int) *AdmissionController {
	return &AdmissionController{
		readSem:  make(chan struct{}, maxReads),
		writeSem: make(chan struct{}, maxWrites),
	}
}

// SetShedThreshold configures the pressure threshold at which active load
// shedding begins. Value is a fraction of pool capacity (0.0–1.0). Zero
// disables shedding (default).
func (ac *AdmissionController) SetShedThreshold(t float64) {
	ac.shedThreshold = t
}

// SetAdmissionWait configures a brief wait duration before rejecting when
// the semaphore is full. Zero means instant rejection (default).
func (ac *AdmissionController) SetAdmissionWait(d time.Duration) {
	ac.admissionWait = d
}

// isWriteMethod reports whether the HTTP method is a write operation.
func isWriteMethod(method string) bool {
	return method == http.MethodPut ||
		method == http.MethodPost ||
		method == http.MethodDelete
}

// Middleware wraps an http.Handler with admission control. Requests that
// exceed the concurrency limit receive 503 SlowDown with Retry-After.
// When a shed threshold is configured, requests may be probabilistically
// rejected before the hard limit based on current pool pressure.
func (ac *AdmissionController) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sem := ac.semFor(r.Method)

		// --- Active load shedding ---
		if ac.shedThreshold > 0 && ac.shouldShed(sem) {
			telemetry.LoadShedTotal.Inc()
			audit.Log(r.Context(), "s3.LoadShed",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			w.Header().Set("Retry-After", "1")
			writeS3Error(w, http.StatusServiceUnavailable, "SlowDown", "Server at capacity")
			return
		}

		// --- Try non-blocking acquire ---
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			next.ServeHTTP(w, r)
			return
		default:
		}

		// --- Brief wait before rejection ---
		if ac.admissionWait > 0 {
			timer := time.NewTimer(ac.admissionWait)
			select {
			case sem <- struct{}{}:
				timer.Stop()
				defer func() { <-sem }()
				next.ServeHTTP(w, r)
				return
			case <-timer.C:
			case <-r.Context().Done():
			}
		}

		telemetry.AdmissionRejectionsTotal.Inc()
		audit.Log(r.Context(), "s3.AdmissionRejected",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
		)
		w.Header().Set("Retry-After", "1")
		writeS3Error(w, http.StatusServiceUnavailable, "SlowDown", "Server at capacity")
	})
}

// shouldShed returns true if the request should be probabilistically
// rejected based on current pool pressure. Shedding probability ramps
// linearly from 0% at the threshold to 100% at full capacity.
func (ac *AdmissionController) shouldShed(sem chan struct{}) bool {
	occupancy := len(sem)
	capacity := cap(sem)
	threshold := int(ac.shedThreshold * float64(capacity))
	if occupancy <= threshold {
		return false
	}
	p := float64(occupancy-threshold) / float64(capacity-threshold)
	return rand.Float64() < p
}

// semFor returns the appropriate semaphore for the given HTTP method.
// When split pools are configured, writes use writeSem and reads use
// readSem. Otherwise the global sem is returned.
func (ac *AdmissionController) semFor(method string) chan struct{} {
	if ac.sem != nil {
		return ac.sem
	}
	if isWriteMethod(method) {
		return ac.writeSem
	}
	return ac.readSem
}

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

package s3api

import (
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// -------------------------------------------------------------------------
// TYPE
// -------------------------------------------------------------------------

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

// -------------------------------------------------------------------------
// CONSTRUCTORS AND CONFIG
// -------------------------------------------------------------------------

// AdmissionLimits shapes how a full pool is handled. ShedThreshold is the
// fraction of pool capacity (0.0-1.0) past which requests are probabilistically
// rejected before the hard limit; Wait is how long to hold a request at a full
// semaphore before rejecting it. Both zero - the useful default - means no
// early shedding and instant rejection.
//
// Passed to the constructor rather than set afterwards. The middleware reads
// these on every request from whichever goroutine served it, so a setter is
// only safe while nothing is being served yet; that was true here by wiring
// order alone, and nothing said so.
type AdmissionLimits struct {
	ShedThreshold float64
	Wait          time.Duration
}

// NewAdmissionControllerFromSem creates an admission controller backed by
// an externally owned semaphore. Use this when background services should
// share the same concurrency budget as HTTP requests.
func NewAdmissionControllerFromSem(sem chan struct{}, lim AdmissionLimits) *AdmissionController {
	return &AdmissionController{
		sem:           sem,
		shedThreshold: lim.ShedThreshold,
		admissionWait: lim.Wait,
	}
}

// NewSplitAdmissionControllerFromSem creates an admission controller backed
// by externally owned read and write semaphores.
func NewSplitAdmissionControllerFromSem(readSem, writeSem chan struct{}, lim AdmissionLimits) *AdmissionController {
	return &AdmissionController{
		readSem:       readSem,
		writeSem:      writeSem,
		shedThreshold: lim.ShedThreshold,
		admissionWait: lim.Wait,
	}
}

// isWriteMethod reports whether the HTTP method is a write operation.
func isWriteMethod(method string) bool {
	return method == http.MethodPut ||
		method == http.MethodPost ||
		method == http.MethodDelete
}

// -------------------------------------------------------------------------
// MIDDLEWARE
// -------------------------------------------------------------------------

// Middleware wraps an http.Handler with admission control. Requests that
// exceed the concurrency limit receive 503 SlowDown with Retry-After.
// When a shed threshold is configured, requests may be probabilistically
// rejected before the hard limit based on current pool pressure.
func (ac *AdmissionController) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sem := ac.semFor(r.Method)

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

		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			next.ServeHTTP(w, r)
			return
		default:
		}

		if ac.admissionWait > 0 {
			timer := time.NewTimer(ac.admissionWait)
			defer timer.Stop()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				next.ServeHTTP(w, r)
				return
			case <-timer.C:
				// Wait elapsed without admission. Fall through to capacity rejection.
			case <-r.Context().Done():
				// Client gave up before we could admit. Don't count this as a
				// server-side rejection or write a response to a closed connection.
				telemetry.AdmissionClientCanceledTotal.Inc()
				return
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

// -------------------------------------------------------------------------
// LOAD SHEDDING
// -------------------------------------------------------------------------

// shouldShed returns true if the request should be probabilistically
// rejected based on current pool pressure. Shedding probability ramps
// linearly from 0% at the threshold to 100% at full capacity. The
// threshold is computed with int() truncation, and shedding begins when
// occupancy reaches or exceeds the threshold (not one above it).
func (ac *AdmissionController) shouldShed(sem chan struct{}) bool {
	occupancy := len(sem)
	capacity := cap(sem)
	threshold := int(ac.shedThreshold * float64(capacity))
	if occupancy < threshold {
		return false
	}
	p := float64(occupancy-threshold) / float64(capacity-threshold)
	return rand.Float64() < p //nolint:gosec // G404: load shed probability does not require crypto-strength randomness
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

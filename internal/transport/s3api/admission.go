// -------------------------------------------------------------------------------
// Admission Controller - Concurrency Shared Fairly Between Virtual Buckets
//
// Author: Alex Freidah
//
// Server-level admission control over a read pool and a write pool, which are
// one pool when they are not split. Each declared virtual bucket's requests
// are a request flow of their own, and every request naming a virtual bucket
// nobody declared shares one, so a name a client makes up cannot open a queue
// of its own. A request that finds its pool full waits in its request flow's
// queue, and a freed slot goes to the request flow holding the fewest, so one
// virtual bucket's bulk import cannot keep the others waiting behind it. A
// request refused a slot is answered 503 SlowDown with a Retry-After header.
// -------------------------------------------------------------------------------

package s3api

import (
	"errors"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/admission"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// TYPE
// -------------------------------------------------------------------------

// BucketChecker reports whether a virtual bucket is declared, counting both
// the config file and the store. *provisioning.Declared satisfies it.
type BucketChecker interface {
	Contains(name string) bool
}

// AdmissionController admits S3 reads from the reads pool and writes from the
// writes pool. wait is how long a request waits for a slot before it is
// refused.
type AdmissionController struct {
	reads   *admission.Pool
	writes  *admission.Pool
	buckets BucketChecker
	wait    time.Duration
}

// NewAdmissionController admits reads from reads and writes from writes, the
// same pool when they are not split, telling virtual buckets apart by buckets.
func NewAdmissionController(reads, writes *admission.Pool, buckets BucketChecker, wait time.Duration) *AdmissionController {
	return &AdmissionController{reads: reads, writes: writes, buckets: buckets, wait: wait}
}

// -------------------------------------------------------------------------
// MIDDLEWARE
// -------------------------------------------------------------------------

// Middleware wraps an http.Handler with admission control. A request refused a
// slot is answered 503 SlowDown with Retry-After, and one whose client gave up
// while it waited is answered nothing and not counted as refused.
func (ac *AdmissionController) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pool := ac.reads
		if isWriteMethod(r.Method) {
			pool = ac.writes
		}
		slot, err := pool.Acquire(r.Context(), ac.requestFlowName(r.URL.Path), ac.wait)
		switch {
		case err == nil:
			defer slot.Release()
			next.ServeHTTP(w, r)
		case r.Context().Err() != nil:
			telemetry.AdmissionClientCanceledTotal.Inc()
		default:
			telemetry.AdmissionRejectionsTotal.WithLabelValues(pool.Name(), rejectionReason(err)).Inc()
			httputil.RefuseThrottled(w, r, WriteS3Error, &admissionRejected)
		}
	})
}

// admissionRejected is how admission turns away a request that found its pool
// full past its wait or its queue.
var admissionRejected = httputil.Throttled{
	Event: "s3.AdmissionRejected", Status: http.StatusServiceUnavailable, Message: "Server at capacity",
}

// isWriteMethod reports whether the HTTP method is a write operation.
func isWriteMethod(method string) bool {
	return method == http.MethodPut ||
		method == http.MethodPost ||
		method == http.MethodDelete
}

// rejectionReason labels why a request was refused a slot: its pool's queues
// were full, or its wait ran out.
func rejectionReason(err error) string {
	if errors.Is(err, admission.ErrQueueFull) {
		return "queue_full"
	}
	return "wait_expired"
}

// requestFlowName names the request flow a request waits in: its virtual
// bucket when one is declared, and otherwise the one every other request
// shares.
func (ac *AdmissionController) requestFlowName(path string) string {
	if bucket, ok := BucketFromPath(path); ok && ac.buckets.Contains(bucket) {
		return bucket
	}
	return ""
}

// -------------------------------------------------------------------------------
// Admission Controller Tests
//
// Author: Alex Freidah
//
// Tests for server-level admission control middleware. Validates that slots
// are given back, a full pool waits and then refuses with 503 and Retry-After,
// split pools do not block each other, and a freed slot goes to the virtual
// bucket holding the fewest.
// -------------------------------------------------------------------------------

package s3api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/admission"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// Names the test pools are labelled by, the paths the tests address, and a
// wait short enough that a full pool refuses at once.
const (
	sharedPool = "shared"
	readPool   = "read"
	writePool  = "write"
	heldPath   = "/test-bucket/key"
	otherPath  = "/test-bucket/key2"
	refuseWait = time.Millisecond
)

// declared is a BucketChecker over the virtual buckets a test declares.
type declared map[string]bool

// Contains reports whether the test declared name.
func (d declared) Contains(name string) bool { return d[name] }

// testBuckets are the virtual buckets these tests address.
var testBuckets = declared{"test-bucket": true, "quiet": true}

// testPool builds an admission pool of capacity slots named name, counted in
// the process metrics as a production pool is.
func testPool(name string, capacity int) *admission.Pool {
	return admission.New(name, capacity, admission.Metrics{
		Queued: telemetry.AdmissionQueued.WithLabelValues(name),
		Waited: telemetry.AdmissionWaitSeconds.WithLabelValues(name),
	})
}

// newAdmission builds a controller over one pool of capacity slots that a
// request waits up to wait for.
func newAdmission(capacity int, wait time.Duration) *AdmissionController {
	pool := testPool(sharedPool, capacity)
	return NewAdmissionController(pool, pool, testBuckets, wait)
}

// blocking is a handler that holds each request until release is closed,
// sending the request's path on entered as it arrives.
type blocking struct {
	entered chan string
	release chan struct{}
	wg      sync.WaitGroup
}

// newBlocking builds a blocking handler with nothing held.
func newBlocking() *blocking {
	return &blocking{entered: make(chan string, 8), release: make(chan struct{})}
}

// ServeHTTP holds the request until release is closed.
func (b *blocking) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.entered <- r.URL.Path
	<-b.release
	w.WriteHeader(http.StatusOK)
}

// start sends a request through h on a goroutine of its own.
func (b *blocking) start(h http.Handler, method, path string) {
	b.wg.Go(func() { serve(context.Background(), h, method, path) })
}

// hold sends a request through h and waits for it to reach the handler.
func (b *blocking) hold(h http.Handler, method, path string) {
	b.start(h, method, path)
	<-b.entered
}

// finish lets every held request return and waits for them.
func (b *blocking) finish() {
	close(b.release)
	b.wg.Wait()
}

// serve sends one request through h and returns its answer.
func serve(ctx context.Context, h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, method, path, nil))
	return rec
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestAdmissionController_ReleasesEachSlot verifies a request gives its slot
// back when it finishes, so a one-slot pool admits requests one after another.
func TestAdmissionController_ReleasesEachSlot(t *testing.T) {
	t.Parallel()
	h := newAdmission(1, refuseWait).Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := range 3 {
		if rec := serve(t.Context(), h, http.MethodGet, heldPath); rec.Code != http.StatusOK {
			t.Errorf("request %d: got %d, want 200", i+1, rec.Code)
		}
	}
}

// TestAdmissionController_RefusesAFullPool verifies a request that finds the
// pool full past its wait is answered 503 with Retry-After, and counted under
// its pool and reason.
func TestAdmissionController_RefusesAFullPool(t *testing.T) {
	t.Parallel()
	b := newBlocking()
	h := newAdmission(1, refuseWait).Middleware(b)
	rejections := telemetry.AdmissionRejectionsTotal.WithLabelValues(sharedPool, "wait_expired")
	before := testutil.ToFloat64(rejections)

	b.hold(h, http.MethodPut, heldPath)
	rec := serve(t.Context(), h, http.MethodPut, otherPath)
	b.finish()

	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Errorf("refused request = %d with Retry-After %q, want 503 with 1", rec.Code, rec.Header().Get("Retry-After"))
	}
	if after := testutil.ToFloat64(rejections); after <= before {
		t.Errorf("AdmissionRejectionsTotal did not increment: before=%v, after=%v", before, after)
	}
}

// TestAdmissionController_SplitPools verifies reads and writes are admitted
// from pools of their own, with DELETE counted as a write: filling one pool
// refuses its next request and leaves the other free.
func TestAdmissionController_SplitPools(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                    string
		reads, writes           int
		held, refused, admitted string
	}{
		{"writes full, reads free", 2, 1, http.MethodPut, http.MethodPut, http.MethodGet},
		{"reads full, writes free", 1, 2, http.MethodGet, http.MethodGet, http.MethodPut},
		{"delete fills the write pool", 2, 1, http.MethodDelete, http.MethodPut, http.MethodGet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ac := NewAdmissionController(testPool(readPool, tc.reads), testPool(writePool, tc.writes), testBuckets, refuseWait)
			b := newBlocking()
			h := ac.Middleware(b)
			defer b.finish()

			b.hold(h, tc.held, heldPath)
			if rec := serve(t.Context(), h, tc.refused, otherPath); rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s on the full pool: got %d, want 503", tc.refused, rec.Code)
			}
			b.hold(h, tc.admitted, heldPath)
		})
	}
}

// TestAdmissionController_WaitAcquiresSlot verifies a request that finds the
// pool full is admitted when a slot frees within its wait.
func TestAdmissionController_WaitAcquiresSlot(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newBlocking()
		h := newAdmission(1, 200*time.Millisecond).Middleware(b)
		b.hold(h, http.MethodGet, heldPath)
		second := make(chan int, 1)
		go func() { second <- serve(t.Context(), h, http.MethodGet, otherPath).Code }()

		time.Sleep(50 * time.Millisecond)
		b.finish()
		if code := <-second; code != http.StatusOK {
			t.Errorf("request after wait: got %d, want 200", code)
		}
	})
}

// TestAdmissionController_ClientCancelDuringWaitNotCountedAsRejection verifies
// a client that gives up while waiting is counted as cancelled and sent
// nothing.
func TestAdmissionController_ClientCancelDuringWaitNotCountedAsRejection(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newBlocking()
		h := newAdmission(1, 2*time.Second).Middleware(b)
		b.hold(h, http.MethodGet, heldPath)
		before := testutil.ToFloat64(telemetry.AdmissionClientCanceledTotal)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- serve(ctx, h, http.MethodGet, otherPath) }()
		synctest.Wait()
		cancel()
		rec := <-done
		b.finish()

		// Other parallel tests share the global counter, so only this delta and
		// the empty response are asserted.
		if after := testutil.ToFloat64(telemetry.AdmissionClientCanceledTotal); after != before+1 {
			t.Errorf("AdmissionClientCanceledTotal did not increment: before=%v after=%v", before, after)
		}
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Errorf("a cancelled client was sent %d %q, want nothing written", rec.Code, rec.Body.String())
		}
	})
}

// TestAdmissionController_FreedSlotGoesToTheQuieterVirtualBucket verifies a
// virtual bucket holding the whole pool does not get the next freed slot ahead
// of one holding none, even though its request queued first.
func TestAdmissionController_FreedSlotGoesToTheQuieterVirtualBucket(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newBlocking()
		h := newAdmission(2, time.Minute).Middleware(b)
		b.hold(h, http.MethodPut, "/test-bucket/a")
		b.hold(h, http.MethodPut, "/test-bucket/b")
		b.start(h, http.MethodPut, "/test-bucket/c")
		synctest.Wait()
		b.start(h, http.MethodPut, "/quiet/d")
		synctest.Wait()

		b.release <- struct{}{}
		synctest.Wait()
		if got := <-b.entered; got != "/quiet/d" {
			t.Errorf("freed slot went to %s, want the virtual bucket holding none", got)
		}
		b.finish()
	})
}

// TestAdmissionController_RequestFlowName verifies each declared virtual
// bucket waits in a request flow of its own, and every other request in one
// they share.
func TestAdmissionController_RequestFlowName(t *testing.T) {
	t.Parallel()
	ac := newAdmission(1, refuseWait)
	for path, want := range map[string]string{
		heldPath:       "test-bucket",
		"/quiet":       "quiet",
		"/made-up/key": "",
		"/":            "",
	} {
		if got := ac.requestFlowName(path); got != want {
			t.Errorf("requestFlowName(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestRejectionReason verifies a refusal is labelled by why it happened.
func TestRejectionReason(t *testing.T) {
	t.Parallel()
	if got := rejectionReason(admission.ErrQueueFull); got != "queue_full" {
		t.Errorf("rejectionReason(ErrQueueFull) = %q, want queue_full", got)
	}
	if got := rejectionReason(admission.ErrWaitExpired); got != "wait_expired" {
		t.Errorf("rejectionReason(ErrWaitExpired) = %q, want wait_expired", got)
	}
}

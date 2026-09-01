// -------------------------------------------------------------------------------
// Admission Controller Tests
//
// Author: Alex Freidah
//
// Tests for server-level admission control middleware. Validates that requests
// within the concurrency limit are allowed, excess requests receive 503 with
// Retry-After, and slots are released after request completion. Covers both
// global and split read/write admission pools.
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

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// newAdmissionFor builds a controller over a semaphore the test owns. Production
// wires the semaphore externally (di.admissionSemFor) so background workers share
// one budget with HTTP requests; these helpers keep that shape without each test
// spelling out the channel.
func newAdmissionFor(maxConcurrent int) *AdmissionController {
	return newAdmissionWithLimits(maxConcurrent, AdmissionLimits{})
}

// newAdmissionWithLimits is newAdmissionFor for the tests that exercise
// shedding or the admission wait, which are constructor arguments rather than
// something a test can set after the fact.
func newAdmissionWithLimits(maxConcurrent int, lim AdmissionLimits) *AdmissionController {
	return NewAdmissionControllerFromSem(make(chan struct{}, maxConcurrent), lim)
}

// newSplitAdmissionFor is newAdmissionFor for the split read/write variant.
func newSplitAdmissionFor(maxReads, maxWrites int) *AdmissionController {
	return NewSplitAdmissionControllerFromSem(
		make(chan struct{}, maxReads), make(chan struct{}, maxWrites), AdmissionLimits{})
}

// TestAdmissionController_AllowsWithinLimit verifies the admission controller allows within limit contract.
// Asserts that request : got , want 200.
func TestAdmissionController_AllowsWithinLimit(t *testing.T) {
	t.Parallel()
	ac := newAdmissionFor(2)

	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Send 2 sequential requests  -  both should succeed
	for i := range 2 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: got %d, want 200", i+1, rec.Code)
		}
	}
}

// TestAdmissionController_RejectsOverLimit verifies the admission controller rejects over limit contract.
// Asserts that second request: got , want 503.
func TestAdmissionController_RejectsOverLimit(t *testing.T) {
	t.Parallel()
	ac := newAdmissionFor(1)

	// entered signals that the handler goroutine has acquired the semaphore.
	// Buffered so the send always succeeds even if the test hasn't reached
	// the receive yet  -  an unbuffered channel + non-blocking select would
	// silently drop the signal under that schedule and deadlock main.
	entered := make(chan struct{}, 1)
	hold := make(chan struct{})
	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-hold
		w.WriteHeader(http.StatusOK)
	}))

	// Start first request  -  will block inside handler
	var wg sync.WaitGroup
	firstDone := make(chan int, 1)
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), "PUT", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
		firstDone <- rec.Code
	})

	// Wait for the first request to enter the handler
	<-entered

	// Second request should be rejected  -  semaphore is full
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequestWithContext(context.Background(), "PUT", "/test-bucket/key2", nil)
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusServiceUnavailable {
		t.Errorf("second request: got %d, want 503", rec2.Code)
	}
	if ra := rec2.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After = %q, want %q", ra, "1")
	}

	// Release the first request
	close(hold)
	wg.Wait()

	code := <-firstDone
	if code != http.StatusOK {
		t.Errorf("first request: got %d, want 200", code)
	}
}

// TestAdmissionController_ReleasesOnCompletion verifies the admission controller releases on completion contract.
// Asserts that first request: got , want 200.
func TestAdmissionController_ReleasesOnCompletion(t *testing.T) {
	t.Parallel()
	ac := newAdmissionFor(1)

	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request completes, freeing the slot
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequestWithContext(context.Background(), "GET", "/test-bucket/key", nil)
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rec1.Code)
	}

	// Second request should succeed because the slot was released
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequestWithContext(context.Background(), "GET", "/test-bucket/key", nil)
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("second request: got %d, want 200", rec2.Code)
	}
}

// TestAdmissionController_IncrementsMetric verifies the admission controller increments metric contract.
// Asserts that status = , want 503.
func TestAdmissionController_IncrementsMetric(t *testing.T) {
	t.Parallel()
	ac := newAdmissionFor(1)

	entered := make(chan struct{}, 1)
	hold := make(chan struct{})
	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-hold
		w.WriteHeader(http.StatusOK)
	}))

	before := testutil.ToFloat64(telemetry.AdmissionRejectionsTotal)

	// Start blocking request
	var wg sync.WaitGroup
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
	})

	// Wait for the request to enter the handler
	<-entered

	// This request should be rejected
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/test-bucket/key2", nil)
	handler.ServeHTTP(rec, req)

	close(hold)
	wg.Wait()

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	after := testutil.ToFloat64(telemetry.AdmissionRejectionsTotal)
	if after <= before {
		t.Errorf("AdmissionRejectionsTotal did not increment: before=%v, after=%v", before, after)
	}
}

// TestSplitAdmission_WriteFull_ReadAllowed verifies the split admission write full read allowed contract.
// Asserts that second write: got , want 503.
func TestSplitAdmission_WriteFull_ReadAllowed(t *testing.T) {
	t.Parallel()
	ac := newSplitAdmissionFor(2, 1)

	entered := make(chan struct{}, 2)
	hold := make(chan struct{})
	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-hold
		w.WriteHeader(http.StatusOK)
	}))

	// Fill the write pool (capacity 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), "PUT", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
	})
	<-entered

	// Another write should be rejected
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "PUT", "/test-bucket/key2", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("second write: got %d, want 503", rec.Code)
	}

	// A read should still succeed  -  separate pool
	readDone := make(chan int, 1)
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
		readDone <- rec.Code
	})
	<-entered

	close(hold)
	wg.Wait()

	if code := <-readDone; code != http.StatusOK {
		t.Errorf("read while writes full: got %d, want 200", code)
	}
}

// TestSplitAdmission_ReadFull_WriteAllowed verifies the split admission read full write allowed contract.
// Asserts that second read: got , want 503.
func TestSplitAdmission_ReadFull_WriteAllowed(t *testing.T) {
	t.Parallel()
	ac := newSplitAdmissionFor(1, 2)

	entered := make(chan struct{}, 2)
	hold := make(chan struct{})
	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-hold
		w.WriteHeader(http.StatusOK)
	}))

	// Fill the read pool (capacity 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
	})
	<-entered

	// Another read should be rejected
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/test-bucket/key2", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("second read: got %d, want 503", rec.Code)
	}

	// A write should still succeed  -  separate pool
	writeDone := make(chan int, 1)
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), "PUT", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
		writeDone <- rec.Code
	})
	<-entered

	close(hold)
	wg.Wait()

	if code := <-writeDone; code != http.StatusOK {
		t.Errorf("write while reads full: got %d, want 200", code)
	}
}

// TestAdmissionController_LoadShedding verifies the admission controller load shedding contract.
// Asserts that shed /1000 at 80 occupancy (threshold 50), expected ~600.
func TestAdmissionController_LoadShedding(t *testing.T) {
	t.Parallel()
	ac := newAdmissionWithLimits(10, AdmissionLimits{ShedThreshold: 0.5})

	// Fill 8 of 10 slots -> 80% occupancy, above 50% threshold
	for range 8 {
		ac.sem <- struct{}{}
	}

	// shouldShed probability: (8-5)/(10-5) = 0.6
	shed := 0
	for range 1000 {
		if ac.shouldShed(ac.sem) {
			shed++
		}
	}

	// Drain slots
	for range 8 {
		<-ac.sem
	}

	// Expect ~600/1000. Allow wide margin for randomness.
	if shed < 400 || shed > 800 {
		t.Errorf("shed %d/1000 at 80%% occupancy (threshold 50%%), expected ~600", shed)
	}
}

// TestAdmissionController_NoSheddingBelowThreshold verifies the admission controller no shedding below threshold path by exercising the shed threshold.
func TestAdmissionController_NoSheddingBelowThreshold(t *testing.T) {
	t.Parallel()
	ac := newAdmissionWithLimits(10, AdmissionLimits{ShedThreshold: 0.8})

	// 0% occupancy, well below 80% threshold  -  should never shed
	for range 100 {
		if ac.shouldShed(ac.sem) {
			t.Fatal("shed at 0%% occupancy with 80%% threshold")
		}
	}
}

// TestAdmissionController_SheddingStartsAtThreshold verifies the admission controller shedding starts at threshold contract.
// Asserts that shed /1000 at exactly threshold occupancy, expected 0.
func TestAdmissionController_SheddingStartsAtThreshold(t *testing.T) {
	t.Parallel()
	// With capacity=10 and threshold=0.5, int(0.5*10) = 5.
	// Shedding should start when occupancy reaches 5 (not 6).
	ac := newAdmissionWithLimits(10, AdmissionLimits{ShedThreshold: 0.5})

	// Fill exactly to threshold (5 of 10)
	for range 5 {
		ac.sem <- struct{}{}
	}

	// At exactly the threshold, shedding probability is:
	// (5-5)/(10-5) = 0.0  -  but since we use < (not <=), occupancy 5
	// is now at-or-above threshold. The probability is 0%, so no
	// shedding occurs at exactly the boundary.
	shed := 0
	for range 1000 {
		if ac.shouldShed(ac.sem) {
			shed++
		}
	}

	// Drain
	for range 5 {
		<-ac.sem
	}

	// At exactly threshold, probability is 0/(10-5) = 0  -  no shedding
	if shed != 0 {
		t.Errorf("shed %d/1000 at exactly threshold occupancy, expected 0", shed)
	}

	// Fill to threshold + 1 (6 of 10)
	for range 6 {
		ac.sem <- struct{}{}
	}

	shed = 0
	for range 1000 {
		if ac.shouldShed(ac.sem) {
			shed++
		}
	}

	for range 6 {
		<-ac.sem
	}

	// Probability: (6-5)/(10-5) = 0.2  -  expect ~200/1000
	if shed < 100 || shed > 350 {
		t.Errorf("shed %d/1000 at threshold+1 occupancy, expected ~200", shed)
	}
}

// TestSplitAdmission_DeleteUsesWritePool verifies the split admission delete uses write pool contract.
// Asserts that PUT while DELETE holds write pool: got , want 503.
func TestSplitAdmission_DeleteUsesWritePool(t *testing.T) {
	t.Parallel()
	ac := newSplitAdmissionFor(2, 1)

	entered := make(chan struct{}, 1)
	hold := make(chan struct{})
	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-hold
		w.WriteHeader(http.StatusOK)
	}))

	// Fill the write pool with a DELETE
	var wg sync.WaitGroup
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), "DELETE", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
	})
	<-entered

	// A PUT should be rejected  -  same pool
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "PUT", "/test-bucket/key2", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("PUT while DELETE holds write pool: got %d, want 503", rec.Code)
	}

	close(hold)
	wg.Wait()
}

// TestAdmissionController_WaitAcquiresSlot verifies the admission controller wait acquires slot contract.
// Asserts that request after wait: got , want 200.
func TestAdmissionController_WaitAcquiresSlot(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ac := newAdmissionWithLimits(1, AdmissionLimits{Wait: 200 * time.Millisecond})

		hold := make(chan struct{})
		entered := make(chan struct{}, 1)
		handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-hold
			w.WriteHeader(http.StatusOK)
		}))

		// Fill the slot
		var wg sync.WaitGroup
		wg.Go(func() {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), "GET", "/test-bucket/key", nil)
			handler.ServeHTTP(rec, req)
		})
		<-entered

		// Release the slot after 50ms  -  well within the 200ms wait window
		secondDone := make(chan int, 1)
		wg.Go(func() {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), "GET", "/test-bucket/key2", nil)
			handler.ServeHTTP(rec, req)
			secondDone <- rec.Code
		})

		time.Sleep(50 * time.Millisecond)
		close(hold)
		wg.Wait()

		if code := <-secondDone; code != http.StatusOK {
			t.Errorf("request after wait: got %d, want 200", code)
		}
	})
}

// TestAdmissionController_WaitTimesOut verifies the admission controller wait times out contract.
// Asserts that timed-out request: got , want 503.
func TestAdmissionController_WaitTimesOut(t *testing.T) {
	t.Parallel()
	ac := newAdmissionWithLimits(1, AdmissionLimits{Wait: 20 * time.Millisecond})

	hold := make(chan struct{})
	entered := make(chan struct{}, 1)
	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-hold
		w.WriteHeader(http.StatusOK)
	}))

	// Fill the slot
	var wg sync.WaitGroup
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
	})
	<-entered

	// Second request  -  slot never frees, should timeout after 20ms
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/test-bucket/key2", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("timed-out request: got %d, want 503", rec.Code)
	}

	close(hold)
	wg.Wait()
}

// TestAdmissionController_ClientCancelDuringWaitNotCountedAsRejection verifies the admission controller client cancel during wait not counted as rejection contract.
// Asserts that AdmissionClientCanceledTotal did not increment: before= after=.
func TestAdmissionController_ClientCancelDuringWaitNotCountedAsRejection(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		// Generous wait so the test reliably observes the client-cancel branch
		// rather than racing the timer.
		ac := newAdmissionWithLimits(1, AdmissionLimits{Wait: 2 * time.Second})

		hold := make(chan struct{})
		entered := make(chan struct{}, 1)
		handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-hold
			w.WriteHeader(http.StatusOK)
		}))

		// Fill the only slot.
		var wg sync.WaitGroup
		wg.Go(func() {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), "GET", "/test-bucket/key", nil)
			handler.ServeHTTP(rec, req)
		})
		<-entered

		beforeCancel := testutil.ToFloat64(telemetry.AdmissionClientCanceledTotal)

		// Second request: cancel its context shortly after dispatch so it lands
		// in the brief-wait branch and exits via r.Context().Done().
		ctx, cancel := context.WithCancel(context.Background())
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx, "GET", "/test-bucket/key2", nil)
		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(rec, req)
			close(done)
		}()
		// synctest.Wait blocks until all goroutines (including the in-flight
		// handler) are parked, so cancel() lands in the wait branch
		// deterministically without a wall-clock sleep.
		synctest.Wait()
		cancel()
		<-done

		// The dedicated client-cancel counter must increment by exactly 1.
		// (Asserting the rejection counter does NOT increment is unreliable here
		// because other parallel tests share the global counter; the directly
		// observable invariants are this delta and the empty response below.)
		if afterCancel := testutil.ToFloat64(telemetry.AdmissionClientCanceledTotal); afterCancel != beforeCancel+1 {
			t.Errorf("AdmissionClientCanceledTotal did not increment: before=%v after=%v",
				beforeCancel, afterCancel)
		}
		// No response should have been written for a cancelled client. httptest
		// defaults Code to 200 when WriteHeader is never called, and Body is
		// empty when Write is never called.
		if rec.Code != http.StatusOK {
			t.Errorf("response status was set despite client cancel: got %d, want 200 (default, not written)", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("response body was written despite client cancel: %q", rec.Body.String())
		}

		close(hold)
		wg.Wait()
	})
}

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

package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestAdmissionController_AllowsWithinLimit(t *testing.T) {
	ac := NewAdmissionController(2)

	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Send 2 sequential requests — both should succeed
	for i := range 2 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: got %d, want 200", i+1, rec.Code)
		}
	}
}

func TestAdmissionController_RejectsOverLimit(t *testing.T) {
	ac := NewAdmissionController(1)

	// entered signals that the handler goroutine has acquired the semaphore
	entered := make(chan struct{})
	hold := make(chan struct{})
	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-hold
		w.WriteHeader(http.StatusOK)
	}))

	// Start first request — will block inside handler
	var wg sync.WaitGroup
	wg.Add(1)
	firstDone := make(chan int, 1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
		firstDone <- rec.Code
	}()

	// Wait for the first request to enter the handler
	<-entered

	// Second request should be rejected — semaphore is full
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("PUT", "/test-bucket/key2", nil)
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

func TestAdmissionController_ReleasesOnCompletion(t *testing.T) {
	ac := NewAdmissionController(1)

	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request completes, freeing the slot
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest("GET", "/test-bucket/key", nil)
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rec1.Code)
	}

	// Second request should succeed because the slot was released
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/test-bucket/key", nil)
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("second request: got %d, want 200", rec2.Code)
	}
}

func TestAdmissionController_IncrementsMetric(t *testing.T) {
	ac := NewAdmissionController(1)

	entered := make(chan struct{})
	hold := make(chan struct{})
	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-hold
		w.WriteHeader(http.StatusOK)
	}))

	before := testutil.ToFloat64(telemetry.AdmissionRejectionsTotal)

	// Start blocking request
	var wg sync.WaitGroup
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
	})

	// Wait for the request to enter the handler
	<-entered

	// This request should be rejected
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket/key2", nil)
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

func TestSplitAdmission_WriteFull_ReadAllowed(t *testing.T) {
	ac := NewSplitAdmissionController(2, 1)

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
		req := httptest.NewRequest("PUT", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
	})
	<-entered

	// Another write should be rejected
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/test-bucket/key2", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("second write: got %d, want 503", rec.Code)
	}

	// A read should still succeed — separate pool
	readDone := make(chan int, 1)
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test-bucket/key", nil)
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

func TestSplitAdmission_ReadFull_WriteAllowed(t *testing.T) {
	ac := NewSplitAdmissionController(1, 2)

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
		req := httptest.NewRequest("GET", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
	})
	<-entered

	// Another read should be rejected
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket/key2", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("second read: got %d, want 503", rec.Code)
	}

	// A write should still succeed — separate pool
	writeDone := make(chan int, 1)
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/test-bucket/key", nil)
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

func TestAdmissionController_LoadShedding(t *testing.T) {
	ac := NewAdmissionController(10)
	ac.SetShedThreshold(0.5)

	// Fill 8 of 10 slots → 80% occupancy, above 50% threshold
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

func TestAdmissionController_NoSheddingBelowThreshold(t *testing.T) {
	ac := NewAdmissionController(10)
	ac.SetShedThreshold(0.8)

	// 0% occupancy, well below 80% threshold — should never shed
	for range 100 {
		if ac.shouldShed(ac.sem) {
			t.Fatal("shed at 0%% occupancy with 80%% threshold")
		}
	}
}

func TestSplitAdmission_DeleteUsesWritePool(t *testing.T) {
	ac := NewSplitAdmissionController(2, 1)

	entered := make(chan struct{})
	hold := make(chan struct{})
	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-hold
		w.WriteHeader(http.StatusOK)
	}))

	// Fill the write pool with a DELETE
	var wg sync.WaitGroup
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("DELETE", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
	})
	<-entered

	// A PUT should be rejected — same pool
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/test-bucket/key2", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("PUT while DELETE holds write pool: got %d, want 503", rec.Code)
	}

	close(hold)
	wg.Wait()
}

func TestAdmissionController_WaitAcquiresSlot(t *testing.T) {
	ac := NewAdmissionController(1)
	ac.SetAdmissionWait(200 * time.Millisecond)

	hold := make(chan struct{})
	entered := make(chan struct{})
	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-hold
		w.WriteHeader(http.StatusOK)
	}))

	// Fill the slot
	var wg sync.WaitGroup
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
	})
	<-entered

	// Release the slot after 50ms — well within the 200ms wait window
	secondDone := make(chan int, 1)
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test-bucket/key2", nil)
		handler.ServeHTTP(rec, req)
		secondDone <- rec.Code
	})

	time.Sleep(50 * time.Millisecond)
	close(hold)
	wg.Wait()

	if code := <-secondDone; code != http.StatusOK {
		t.Errorf("request after wait: got %d, want 200", code)
	}
}

func TestAdmissionController_WaitTimesOut(t *testing.T) {
	ac := NewAdmissionController(1)
	ac.SetAdmissionWait(20 * time.Millisecond)

	hold := make(chan struct{})
	entered := make(chan struct{})
	handler := ac.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-hold
		w.WriteHeader(http.StatusOK)
	}))

	// Fill the slot
	var wg sync.WaitGroup
	wg.Go(func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
	})
	<-entered

	// Second request — slot never frees, should timeout after 20ms
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test-bucket/key2", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("timed-out request: got %d, want 503", rec.Code)
	}

	close(hold)
	wg.Wait()
}

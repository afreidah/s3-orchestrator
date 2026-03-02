// -------------------------------------------------------------------------------
// Admission Controller Tests
//
// Author: Alex Freidah
//
// Tests for server-level admission control middleware. Validates that requests
// within the concurrency limit are allowed, excess requests receive 503, and
// slots are released after request completion.
// -------------------------------------------------------------------------------

package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

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
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test-bucket/key", nil)
		handler.ServeHTTP(rec, req)
	}()

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

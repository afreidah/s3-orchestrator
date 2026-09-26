// -------------------------------------------------------------------------------
// CircuitBreakerBackend Tests
//
// Author: Alex Freidah
//
// Tests for the per-backend circuit breaker wrapper: all 4 backend.ObjectBackend methods
// forward correctly when closed, return breaker.ErrBackendUnavailable when open, only
// backend-health failures open the circuit, client requests never probe it, and
// Unwrap() returns the inner backend for type assertions.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// newTestCBBackend constructs a new test cbbackend.
func newTestCBBackend(mock *mockBackend, threshold int, timeout time.Duration) *CircuitBreakerBackend {
	return NewCircuitBreakerBackend(mock, CircuitBreakerConfig{Name: "test-backend", Threshold: threshold, Timeout: timeout})
}

// -------------------------------------------------------------------------
// Forwarding when closed
// -------------------------------------------------------------------------

// TestCBBackend_PutObject_Forwards verifies the cbbackend put object forwards contract.
// Asserts that PutObject:.
func TestCBBackend_PutObject_Forwards(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	etag, err := cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if etag == "" {
		t.Fatal("expected non-empty etag")
	}
}

// TestCBBackend_GetObject_Forwards verifies the cbbackend get object forwards contract.
// Asserts that GetObject:.
func TestCBBackend_GetObject_Forwards(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	_, _ = cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)

	result, err := cb.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	_ = result.Body.Close()
	if result.Size != 4 {
		t.Fatalf("expected size 4, got %d", result.Size)
	}
}

// TestCBBackend_HeadObject_Forwards verifies the cbbackend head object forwards contract.
// Asserts that HeadObject:.
func TestCBBackend_HeadObject_Forwards(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	_, _ = cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)

	result, err := cb.HeadObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if result.Size != 4 {
		t.Fatalf("expected size 4, got %d", result.Size)
	}
}

// TestCBBackend_DeleteObject_Forwards verifies the cbbackend delete object forwards contract.
// Asserts that DeleteObject:.
func TestCBBackend_DeleteObject_Forwards(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	_, _ = cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)

	if err := cb.DeleteObject(context.Background(), "key"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	// Verify it's deleted
	mock.mu.Lock()
	_, exists := mock.objects["key"]
	mock.mu.Unlock()
	if exists {
		t.Fatal("object should be deleted")
	}
}

// -------------------------------------------------------------------------
// Circuit open
// -------------------------------------------------------------------------

// TestCBBackend_PutObject_CircuitOpen verifies the cbbackend put object circuit open contract.
// Asserts that expected breaker.ErrBackendUnavailable, got.
func TestCBBackend_PutObject_CircuitOpen(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.putErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)

	// Trip the circuit
	_, _ = cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)

	// Next call should return breaker.ErrBackendUnavailable without hitting mock
	_, err := cb.PutObject(context.Background(), "key2", strings.NewReader("data"), 4, "text/plain", nil)
	if !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Fatalf("expected breaker.ErrBackendUnavailable, got %v", err)
	}
}

// TestCBBackend_GetObject_CircuitOpen verifies the cbbackend get object circuit open contract.
// Asserts that expected breaker.ErrBackendUnavailable, got.
func TestCBBackend_GetObject_CircuitOpen(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.getErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)

	// Trip the circuit
	_, _ = cb.GetObject(context.Background(), "key", "")

	_, err := cb.GetObject(context.Background(), "key", "")
	if !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Fatalf("expected breaker.ErrBackendUnavailable, got %v", err)
	}
}

// TestCBBackend_HeadObject_CircuitOpen verifies the cbbackend head object circuit open contract.
// Asserts that expected breaker.ErrBackendUnavailable, got.
func TestCBBackend_HeadObject_CircuitOpen(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.headErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)

	_, _ = cb.HeadObject(context.Background(), "key")

	_, err := cb.HeadObject(context.Background(), "key")
	if !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Fatalf("expected breaker.ErrBackendUnavailable, got %v", err)
	}
}

// TestCBBackend_DeleteObject_CircuitOpen verifies the cbbackend delete object circuit open contract.
// Asserts that expected breaker.ErrBackendUnavailable, got.
func TestCBBackend_DeleteObject_CircuitOpen(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.delErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)

	_ = cb.DeleteObject(context.Background(), "key")

	err := cb.DeleteObject(context.Background(), "key")
	if !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Fatalf("expected breaker.ErrBackendUnavailable, got %v", err)
	}
}

// -------------------------------------------------------------------------
// Recovery
// -------------------------------------------------------------------------

// TestCBBackend_ClientRequestsNeverProbe verifies that once open, the breaker
// refuses every client request even after the open timeout, and closes only
// through Recover.
func TestCBBackend_ClientRequestsNeverProbe(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		mock := newMockBackend()
		mock.putErr = errors.New("connection refused")
		cb := newTestCBBackend(mock, 1, 10*time.Millisecond)

		_, _ = cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)
		time.Sleep(time.Second)

		mock.mu.Lock()
		mock.putErr = nil
		mock.mu.Unlock()

		_, err := cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)
		if !errors.Is(err, breaker.ErrBackendUnavailable) {
			t.Fatalf("PutObject after the open timeout = %v, want ErrBackendUnavailable", err)
		}
		if cb.State() != breaker.StateOpen {
			t.Errorf("state = %v, want open: client requests are never probes", cb.State())
		}

		cb.Recover()
		if _, err := cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil); err != nil {
			t.Fatalf("PutObject after Recover: %v", err)
		}
	})
}

// TestCBBackend_OpeningErrorKeepsCause verifies the error that opens the
// circuit still matches ErrBackendUnavailable and carries the backend's own
// error, so logs show what failed.
func TestCBBackend_OpeningErrorKeepsCause(t *testing.T) {
	t.Parallel()
	cause := &httpError{code: 503, msg: "SlowDown"}
	mock := newMockBackend()
	mock.getErr = cause
	cb := newTestCBBackend(mock, 1, time.Minute)

	_, err := cb.GetObject(context.Background(), "key", "")
	if !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Errorf("err = %v, want it to match ErrBackendUnavailable", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("err = %v, want it to wrap the backend error", err)
	}
}

// TestCBBackend_RequestErrorsDoNotOpen verifies that a healthy backend
// answering request-specific 4xx errors - a range past the end of a zero-byte
// object, a failed precondition - stays closed however many arrive.
func TestCBBackend_RequestErrorsDoNotOpen(t *testing.T) {
	t.Parallel()
	for _, code := range []int{400, 409, 412, 416} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			mock := newMockBackend()
			mock.getErr = &httpError{code: code, msg: "request error"}
			cb := newTestCBBackend(mock, 3, time.Minute)

			for range 10 {
				_, _ = cb.GetObject(context.Background(), "key", "bytes=0-31")
			}
			if cb.State() != breaker.StateClosed {
				t.Errorf("state after ten %d responses = %v, want closed", code, cb.State())
			}
		})
	}
}

// -------------------------------------------------------------------------
// Unwrap
// -------------------------------------------------------------------------

// TestCBBackend_Unwrap verifies the cbbackend unwrap path by exercising cb.Unwrap.
func TestCBBackend_Unwrap(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	inner := cb.Unwrap()
	if inner != mock {
		t.Fatal("Unwrap should return the inner backend")
	}
}

// TestCBBackend_CheckHealthBypassesOpenCircuit verifies the health check
// reaches the backend while the circuit is open.
func TestCBBackend_CheckHealthBypassesOpenCircuit(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.getErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)
	_, _ = cb.GetObject(context.Background(), "key", "")

	if err := cb.CheckHealth(context.Background()); err != nil {
		t.Errorf("CheckHealth on an open circuit = %v, want the backend's nil", err)
	}
	mock.mu.Lock()
	mock.bucketErr = errors.New("still down")
	mock.mu.Unlock()
	if err := cb.CheckHealth(context.Background()); err == nil {
		t.Error("CheckHealth = nil, want the backend's failure")
	}
}

// httpError is a test helper that satisfies the HTTPStatusCode() interface.
type httpError struct {
	code int
	msg  string
}

// Error returns the error message.
func (e *httpError) Error() string { return e.msg }

// HTTPStatusCode satisfies the smithy http.ResponseError
// interface used by the awserr-style typed-error tests below.
func (e *httpError) HTTPStatusCode() int { return e.code }

// TestCBBackend_404DoesNotTripBreaker verifies the cbbackend 404 does not trip breaker contract.
// Asserts that expected success after 404s, got error:.
func TestCBBackend_404DoesNotTripBreaker(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	// 3 consecutive 404 errors should NOT trip the circuit breaker
	mock.getErr = &httpError{code: 404, msg: "NoSuchKey"}
	for range 3 {
		_, _ = cb.GetObject(context.Background(), "missing-key", "")
	}

	// Circuit should still be closed  -  next call should reach the backend
	mock.getErr = nil
	mock.objects = map[string]mockObject{"exists": {data: []byte("data")}}
	result, err := cb.GetObject(context.Background(), "exists", "")
	if err != nil {
		t.Fatalf("expected success after 404s, got error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	if cb.State() != breaker.StateClosed {
		t.Errorf("expected circuit closed, got %v", cb.State())
	}
}

// TestCBBackend_500DoesTripsBreaker verifies the cbbackend 500 does trips breaker contract.
// Asserts that expected circuit open after 500s, got.
func TestCBBackend_500DoesTripsBreaker(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	// 3 consecutive 500 errors SHOULD trip the circuit breaker
	mock.getErr = &httpError{code: 500, msg: "InternalServerError"}
	for range 3 {
		_, _ = cb.GetObject(context.Background(), "key", "")
	}

	if cb.State() != breaker.StateOpen {
		t.Errorf("expected circuit open after 500s, got %v", cb.State())
	}

	// Next call should return ErrBackendUnavailable without reaching backend
	_, err := cb.GetObject(context.Background(), "key", "")
	if !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Errorf("expected ErrBackendUnavailable, got %v", err)
	}
}

// TestIsBackendError_Classification verifies which failures count against a
// backend: no status, 5xx, 429, and credential rejections do; any other
// status is a request-specific answer from a working backend and does not.
func TestIsBackendError_Classification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"no status", errors.New("connection refused"), true},
		{"500", &httpError{code: 500}, true},
		{"503 wrapped", fmt.Errorf("get object failed: %w", &httpError{code: 503}), true},
		{"429", &httpError{code: 429}, true},
		{"401", &httpError{code: 401}, true},
		{"403", &httpError{code: 403}, true},
		{"400", &httpError{code: 400}, false},
		{"404", &httpError{code: 404}, false},
		{"409", &httpError{code: 409}, false},
		{"412", &httpError{code: 412}, false},
		{"416 wrapped", fmt.Errorf("get object failed: %w", &httpError{code: 416}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isBackendError(tc.err); got != tc.want {
				t.Errorf("isBackendError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsBackendError_ContextErrors verifies caller-side cancellation and
// deadline (including wrapped) never count toward a backend's failure
// threshold: they signal a timeout/shutdown, not the backend's health, and
// counting them is what let a slow source cascade-trip healthy backends.
func TestIsBackendError_ContextErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"canceled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
		{"wrapped canceled", fmt.Errorf("put object failed: %w", context.Canceled)},
		{"wrapped deadline", fmt.Errorf("write to target: %w", context.DeadlineExceeded)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if isBackendError(tc.err) {
				t.Errorf("%v should not count as a backend error", tc.err)
			}
		})
	}
}

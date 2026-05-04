// -------------------------------------------------------------------------------
// CircuitBreakerBackend Tests
//
// Author: Alex Freidah
//
// Tests for the per-backend circuit breaker wrapper: all 4 backend.ObjectBackend methods
// forward correctly when closed, return breaker.ErrBackendUnavailable when open, and
// Unwrap() returns the inner backend for type assertions.
// -------------------------------------------------------------------------------

package backend

import (
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// newTestCBBackend constructs a new test cbbackend.
func newTestCBBackend(mock *mockBackend, threshold int, timeout time.Duration) *CircuitBreakerBackend {
	return NewCircuitBreakerBackend(mock, "test-backend", threshold, timeout)
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

// TestCBBackend_RecoveryAfterTimeout verifies the cbbackend recovery after timeout contract.
// Asserts that probe should succeed:.
func TestCBBackend_RecoveryAfterTimeout(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.putErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, 10*time.Millisecond)

	// Trip the circuit
	_, _ = cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)

	time.Sleep(15 * time.Millisecond)

	// Fix the mock
	mock.mu.Lock()
	mock.putErr = nil
	mock.mu.Unlock()

	// Probe should succeed, circuit closes
	_, err := cb.PutObject(context.Background(), "key", strings.NewReader("data"), 4, "text/plain", nil)
	if err != nil {
		t.Fatalf("probe should succeed: %v", err)
	}
	if !cb.IsHealthy() {
		t.Fatal("circuit should be closed after successful probe")
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

// TestCBBackend_NestedUnwrap verifies the cbbackend nested unwrap path by exercising cb2.Unwrap, u.Unwrap.
func TestCBBackend_NestedUnwrap(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	cb1 := newTestCBBackend(mock, 3, time.Minute)
	cb2 := NewCircuitBreakerBackend(cb1, "outer", 3, time.Minute)

	// Unwrap one layer
	inner := cb2.Unwrap()
	if inner != cb1 {
		t.Fatal("first Unwrap should return cb1")
	}

	// Unwrap fully (like SyncBackend does)
	var be ObjectBackend = cb2
	for {
		if u, ok := be.(interface{ Unwrap() ObjectBackend }); ok {
			be = u.Unwrap()
		} else {
			break
		}
	}
	if be != mock {
		t.Fatal("full unwrap should return the mock backend")
	}
}

// httpError is a test helper that satisfies the HTTPStatusCode() interface.
type httpError struct {
	code int
	msg  string
}

// Error returns the error message.
func (e *httpError) Error() string       { return e.msg }
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

// TestIsBackendError_404 verifies the is backend error 404 behaviour described by the test name.
func TestIsBackendError_404(t *testing.T) {
	t.Parallel()
	if isBackendError(&httpError{code: 404, msg: "NoSuchKey"}) {
		t.Error("404 should not be a backend error")
	}
}

// TestIsBackendError_500 verifies the is backend error 500 behaviour described by the test name.
func TestIsBackendError_500(t *testing.T) {
	t.Parallel()
	if !isBackendError(&httpError{code: 500, msg: "InternalServerError"}) {
		t.Error("500 should be a backend error")
	}
}

// TestIsBackendError_Nil verifies the is backend error nil behaviour described by the test name.
func TestIsBackendError_Nil(t *testing.T) {
	t.Parallel()
	if isBackendError(nil) {
		t.Error("nil should not be a backend error")
	}
}

// TestIsBackendError_PlainError verifies the is backend error plain error path by exercising errors.New.
func TestIsBackendError_PlainError(t *testing.T) {
	t.Parallel()
	if !isBackendError(errors.New("connection refused")) {
		t.Error("plain error should be a backend error")
	}
}

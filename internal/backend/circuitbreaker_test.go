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

func newTestCBBackend(mock *mockBackend, threshold int, timeout time.Duration) *CircuitBreakerBackend {
	return NewCircuitBreakerBackend(mock, "test-backend", threshold, timeout)
}

// -------------------------------------------------------------------------
// Forwarding when closed
// -------------------------------------------------------------------------

func TestCBBackend_PutObject_Forwards(t *testing.T) {
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

func TestCBBackend_GetObject_Forwards(t *testing.T) {
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

func TestCBBackend_HeadObject_Forwards(t *testing.T) {
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

func TestCBBackend_DeleteObject_Forwards(t *testing.T) {
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

func TestCBBackend_PutObject_CircuitOpen(t *testing.T) {
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

func TestCBBackend_GetObject_CircuitOpen(t *testing.T) {
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

func TestCBBackend_HeadObject_CircuitOpen(t *testing.T) {
	mock := newMockBackend()
	mock.headErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, time.Minute)

	_, _ = cb.HeadObject(context.Background(), "key")

	_, err := cb.HeadObject(context.Background(), "key")
	if !errors.Is(err, breaker.ErrBackendUnavailable) {
		t.Fatalf("expected breaker.ErrBackendUnavailable, got %v", err)
	}
}

func TestCBBackend_DeleteObject_CircuitOpen(t *testing.T) {
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

func TestCBBackend_RecoveryAfterTimeout(t *testing.T) {
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

func TestCBBackend_Unwrap(t *testing.T) {
	mock := newMockBackend()
	cb := newTestCBBackend(mock, 3, time.Minute)

	inner := cb.Unwrap()
	if inner != mock {
		t.Fatal("Unwrap should return the inner backend")
	}
}

func TestCBBackend_NestedUnwrap(t *testing.T) {
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

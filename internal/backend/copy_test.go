// -------------------------------------------------------------------------------
// Backend Copy Capability Tests
//
// Author: Alex Freidah
//
// Verifies the BackendCopier capability interface and the
// CircuitBreakerBackend's CopyObject forwarding behavior. Pins the
// fallback contract for decorators whose underlying backend lacks
// native copy support.
// -------------------------------------------------------------------------------

package backend

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// copyingMockBackend embeds *mockBackend and adds a CopyObject method so
// it satisfies BackendCopier. Counts calls and exposes an injectable
// error so tests can drive success and failure paths without spinning
// up a real S3 server.
type copyingMockBackend struct {
	*mockBackend
	copyCalls int
	copyErr   error
}

func newCopyingMockBackend() *copyingMockBackend {
	return &copyingMockBackend{mockBackend: newMockBackend()}
}

// CopyObject records the call and either returns the injected error or
// copies the source object's bytes in-memory so existence checks pass.
func (c *copyingMockBackend) CopyObject(_ context.Context, srcKey, dstKey, _ string, _ map[string]string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.copyCalls++
	if c.copyErr != nil {
		return "", c.copyErr
	}
	src, ok := c.objects[srcKey]
	if !ok {
		return "", errors.New("source not found")
	}
	c.objects[dstKey] = src
	return src.etag, nil
}

// TestCircuitBreakerBackend_CopyObject_ForwardsWhenSupported verifies
// that a wrapped backend implementing BackendCopier receives the call
// through the circuit breaker.
func TestCircuitBreakerBackend_CopyObject_ForwardsWhenSupported(t *testing.T) {
	t.Parallel()
	inner := newCopyingMockBackend()
	if _, err := inner.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "", nil); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	cb := NewCircuitBreakerBackend(inner, CircuitBreakerConfig{Name: "test", Threshold: 3, Timeout: time.Minute})
	etag, err := cb.CopyObject(context.Background(), "src", "dst", "", nil)
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if etag == "" {
		t.Errorf("expected non-empty etag")
	}
	if inner.copyCalls != 1 {
		t.Errorf("copyCalls = %d, want 1", inner.copyCalls)
	}
}

// TestCircuitBreakerBackend_CopyObject_ReturnsNotSupportedWhenInnerLacksIt
// pins the fallback contract: a decorator wrapping a backend that does
// not implement BackendCopier must return ErrCopyNotSupported so the
// proxy can fall back to materialized copy.
func TestCircuitBreakerBackend_CopyObject_ReturnsNotSupportedWhenInnerLacksIt(t *testing.T) {
	t.Parallel()
	inner := newMockBackend() // does not implement BackendCopier
	cb := NewCircuitBreakerBackend(inner, CircuitBreakerConfig{Name: "test", Threshold: 3, Timeout: time.Minute})
	_, err := cb.CopyObject(context.Background(), "src", "dst", "", nil)
	if !errors.Is(err, ErrCopyNotSupported) {
		t.Fatalf("err = %v, want ErrCopyNotSupported", err)
	}
}

// TestCircuitBreakerBackend_CopyObject_FailureTripsBreaker pins that
// CopyObject failures contribute to the circuit breaker's failure
// count, so a misbehaving backend's native copy path trips the breaker
// just like its other operations.
func TestCircuitBreakerBackend_CopyObject_FailureTripsBreaker(t *testing.T) {
	t.Parallel()
	inner := newCopyingMockBackend()
	inner.copyErr = errors.New("backend on fire")

	cb := NewCircuitBreakerBackend(inner, CircuitBreakerConfig{Name: "test", Threshold: 1, Timeout: time.Minute})
	if _, err := cb.CopyObject(context.Background(), "src", "dst", "", nil); err == nil {
		t.Fatal("expected error on first call")
	}
	// Next call should be short-circuited by the breaker.
	_, err := cb.CopyObject(context.Background(), "src", "dst", "", nil)
	if err == nil {
		t.Fatal("expected breaker to be open")
	}
	if inner.copyCalls != 1 {
		t.Errorf("inner copyCalls = %d, want 1 (second call short-circuited)", inner.copyCalls)
	}
}

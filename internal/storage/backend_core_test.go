// -------------------------------------------------------------------------------
// Backend Core Tests - Circuit Breaker Filtering
//
// Author: Alex Freidah
//
// Unit tests for excludeUnhealthy which filters circuit-broken backends from
// the eligible list during write routing.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExcludeUnhealthy_FiltersOpenCircuitBreaker(t *testing.T) {
	healthy := NewCircuitBreakerBackend(newMockBackend(), "healthy", 3, 15*time.Second)

	failingMock := newMockBackend()
	failingMock.putErr = errors.New("backend down")
	unhealthy := NewCircuitBreakerBackend(failingMock, "unhealthy", 1, 15*time.Second)

	// Trip the unhealthy backend's circuit breaker
	_, _ = unhealthy.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)

	core := &backendCore{
		backends: map[string]ObjectBackend{
			"healthy":   healthy,
			"unhealthy": unhealthy,
		},
	}

	eligible := core.excludeUnhealthy([]string{"healthy", "unhealthy"})
	if len(eligible) != 1 {
		t.Fatalf("expected 1 eligible backend, got %d", len(eligible))
	}
	if eligible[0] != "healthy" {
		t.Errorf("expected 'healthy', got %q", eligible[0])
	}
}

func TestExcludeUnhealthy_AllHealthy(t *testing.T) {
	b1 := NewCircuitBreakerBackend(newMockBackend(), "b1", 3, 15*time.Second)
	b2 := NewCircuitBreakerBackend(newMockBackend(), "b2", 3, 15*time.Second)

	core := &backendCore{
		backends: map[string]ObjectBackend{
			"b1": b1,
			"b2": b2,
		},
	}

	eligible := core.excludeUnhealthy([]string{"b1", "b2"})
	if len(eligible) != 2 {
		t.Fatalf("expected 2 eligible backends, got %d", len(eligible))
	}
}

func TestExcludeUnhealthy_AllUnhealthy_TimeoutNotElapsed(t *testing.T) {
	failingMock1 := newMockBackend()
	failingMock1.putErr = errors.New("backend down")
	b1 := NewCircuitBreakerBackend(failingMock1, "b1", 1, 15*time.Second)

	failingMock2 := newMockBackend()
	failingMock2.putErr = errors.New("backend down")
	b2 := NewCircuitBreakerBackend(failingMock2, "b2", 1, 15*time.Second)

	// Trip both
	_, _ = b1.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)
	_, _ = b2.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)

	core := &backendCore{
		backends: map[string]ObjectBackend{
			"b1": b1,
			"b2": b2,
		},
	}

	// Before timeout elapses, all should be filtered out
	eligible := core.excludeUnhealthy([]string{"b1", "b2"})
	if len(eligible) != 0 {
		t.Fatalf("expected 0 eligible backends before timeout, got %d", len(eligible))
	}
}

func TestExcludeUnhealthy_AllUnhealthy_ProbeEligible(t *testing.T) {
	failingMock1 := newMockBackend()
	failingMock1.putErr = errors.New("backend down")
	b1 := NewCircuitBreakerBackend(failingMock1, "b1", 1, 1*time.Millisecond)

	failingMock2 := newMockBackend()
	failingMock2.putErr = errors.New("backend down")
	b2 := NewCircuitBreakerBackend(failingMock2, "b2", 1, 1*time.Millisecond)

	// Trip both
	_, _ = b1.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)
	_, _ = b2.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)

	// Wait for open timeout to elapse
	time.Sleep(5 * time.Millisecond)

	core := &backendCore{
		backends: map[string]ObjectBackend{
			"b1": b1,
			"b2": b2,
		},
	}

	// After timeout elapses, probe-eligible backends should be allowed through
	eligible := core.excludeUnhealthy([]string{"b1", "b2"})
	if len(eligible) != 2 {
		t.Fatalf("expected 2 probe-eligible backends after timeout, got %d", len(eligible))
	}
}

func TestExcludeUnhealthy_HalfOpenAllowedForProbe(t *testing.T) {
	failingMock := newMockBackend()
	failingMock.putErr = errors.New("backend down")
	// Use a tiny open timeout so we can transition to half-open immediately
	b := NewCircuitBreakerBackend(failingMock, "probe", 1, 1*time.Millisecond)

	// Trip the circuit breaker
	_, _ = b.PutObject(context.TODO(), "key", strings.NewReader("x"), 1, "", nil)
	if b.State() != stateOpen {
		t.Fatalf("expected open state, got %s", b.State())
	}

	// Wait for the open timeout to elapse so PreCheck transitions to half-open
	time.Sleep(5 * time.Millisecond)
	_ = b.PreCheck()
	if b.State() != stateHalfOpen {
		t.Fatalf("expected half-open state, got %s", b.State())
	}

	core := &backendCore{
		backends: map[string]ObjectBackend{
			"probe": b,
		},
	}

	eligible := core.excludeUnhealthy([]string{"probe"})
	if len(eligible) != 1 {
		t.Fatalf("expected half-open backend to be eligible for probe, got %d", len(eligible))
	}
}

func TestExcludeUnhealthy_NonCBBackendsAlwaysEligible(t *testing.T) {
	core := &backendCore{
		backends: map[string]ObjectBackend{
			"plain": newMockBackend(),
		},
	}

	eligible := core.excludeUnhealthy([]string{"plain"})
	if len(eligible) != 1 {
		t.Fatalf("expected 1 eligible backend, got %d", len(eligible))
	}
}

func TestExcludeUnhealthy_UnknownBackendSkipped(t *testing.T) {
	core := &backendCore{
		backends: map[string]ObjectBackend{},
	}

	eligible := core.excludeUnhealthy([]string{"missing"})
	if len(eligible) != 0 {
		t.Fatalf("expected 0 eligible backends, got %d", len(eligible))
	}
}

// -------------------------------------------------------------------------
// withTimeout — deadline cascading
// -------------------------------------------------------------------------

func TestWithTimeout_NoParentDeadline(t *testing.T) {
	core := &backendCore{backendTimeout: 5 * time.Second}
	ctx, cancel := core.withTimeout(context.Background())
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline to be set")
	}
	remaining := time.Until(deadline)
	if remaining < 4*time.Second || remaining > 6*time.Second {
		t.Errorf("expected ~5s deadline, got %v", remaining)
	}
}

func TestWithTimeout_ParentTighter(t *testing.T) {
	// Parent has a 1s deadline; backend timeout is 30s.
	// The tighter parent deadline should be preserved.
	parent, parentCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer parentCancel()

	core := &backendCore{backendTimeout: 30 * time.Second}
	ctx, cancel := core.withTimeout(parent)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline to be set")
	}
	remaining := time.Until(deadline)
	if remaining > 2*time.Second {
		t.Errorf("expected parent's ~1s deadline to be preserved, got %v", remaining)
	}
}

func TestWithTimeout_BackendTighter(t *testing.T) {
	// Parent has a 30s deadline; backend timeout is 1s.
	// The tighter backend timeout should be applied.
	parent, parentCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer parentCancel()

	core := &backendCore{backendTimeout: 1 * time.Second}
	ctx, cancel := core.withTimeout(parent)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline to be set")
	}
	remaining := time.Until(deadline)
	if remaining > 2*time.Second {
		t.Errorf("expected backend's ~1s timeout to be applied, got %v", remaining)
	}
}

func TestWithTimeout_ZeroTimeout(t *testing.T) {
	core := &backendCore{backendTimeout: 0}
	ctx, cancel := core.withTimeout(context.Background())
	defer cancel()

	if _, ok := ctx.Deadline(); ok {
		t.Error("expected no deadline when backendTimeout is 0")
	}
}

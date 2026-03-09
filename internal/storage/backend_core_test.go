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

func TestExcludeUnhealthy_AllUnhealthy(t *testing.T) {
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

	eligible := core.excludeUnhealthy([]string{"b1", "b2"})
	if len(eligible) != 0 {
		t.Fatalf("expected 0 eligible backends, got %d", len(eligible))
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

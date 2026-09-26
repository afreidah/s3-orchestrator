// -------------------------------------------------------------------------------
// Breaker Registry Tests
//
// Author: Alex Freidah
//
// Verifies the registry the watchdog probes on each tick. Tests cover
// registration, nil filtering, a safe no-op when the registry is empty, and
// that the real *CircuitBreaker satisfies Prober without an adapter.
// -------------------------------------------------------------------------------

package breaker

import (
	"context"
	"sync/atomic"
	"testing"
)

// fakeProber counts Probe calls; atomic because ProbeAll runs them
// concurrently.
type fakeProber struct {
	count atomic.Int32
}

// Probe records that the registry probed this breaker.
func (f *fakeProber) Probe(context.Context) {
	f.count.Add(1)
}

// TestNewRegistry_DropsNil verifies that nil entries passed to NewRegistry
// are filtered out instead of panicking later.
func TestNewRegistry_DropsNil(t *testing.T) {
	r := NewRegistry(nil, &fakeProber{}, nil)
	if got := r.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
}

// TestRegistry_RegisterAndProbe verifies that ProbeAll probes every
// registered breaker exactly once per call.
func TestRegistry_RegisterAndProbe(t *testing.T) {
	a, b, c := &fakeProber{}, &fakeProber{}, &fakeProber{}
	r := NewRegistry(a)
	r.Register(b)
	r.Register(c)
	r.Register(nil) // no-op

	if got := r.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}

	r.ProbeAll(t.Context())
	r.ProbeAll(t.Context())

	for i, f := range []*fakeProber{a, b, c} {
		if got := f.count.Load(); got != 2 {
			t.Errorf("breaker %d probed %d times, want 2", i, got)
		}
	}
}

// TestRegistry_EmptyProbeIsNoOp verifies that ProbeAll on an empty registry
// does not panic.
func TestRegistry_EmptyProbeIsNoOp(t *testing.T) {
	r := NewRegistry()
	r.ProbeAll(t.Context()) // must not panic
	if r.Len() != 0 {
		t.Fatalf("Len = %d, want 0", r.Len())
	}
}

// TestRegistry_RealCircuitBreakerSatisfiesInterface verifies that a real
// *CircuitBreaker can be registered without type assertions.
func TestRegistry_RealCircuitBreakerSatisfiesInterface(t *testing.T) {
	cb := NewCircuitBreaker(Config{Name: "test", Threshold: 1, Timeout: 0, IsError: func(error) bool { return true }, Sentinel: ErrBackendUnavailable})
	r := NewRegistry(cb)
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
	r.ProbeAll(t.Context()) // must not panic on a closed circuit
}

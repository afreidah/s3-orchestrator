// -------------------------------------------------------------------------------
// Breaker Registry Tests
//
// Author: Alex Freidah
//
// Verifies the per-process registry that hot-reload code uses to reset every
// known circuit breaker after a config change. Tests cover registration, nil
// filtering, idempotent resets when the registry is empty, and that the real
// *CircuitBreaker satisfies the Resetter interface so the reload path does
// not depend on a hand-rolled adapter.
// -------------------------------------------------------------------------------

package breaker

import "testing"

// fakeResetter is a counting Resetter used to assert that the
// registry calls ResetStaleProbe on every registered breaker.
type fakeResetter struct {
	count int
	ret   bool
}

// ResetStaleProbe records that the registry called the resetter
// and returns the test-configured boolean as the probe result.
func (f *fakeResetter) ResetStaleProbe() bool {
	f.count++
	return f.ret
}

// TestNewRegistry_DropsNil verifies that nil entries passed to NewRegistry
// are filtered out instead of panicking later.
func TestNewRegistry_DropsNil(t *testing.T) {
	r := NewRegistry(nil, &fakeResetter{}, nil)
	if got := r.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
}

// TestRegistry_RegisterAndReset verifies that ResetStaleProbes calls every
// registered breaker exactly once.
func TestRegistry_RegisterAndReset(t *testing.T) {
	a, b, c := &fakeResetter{}, &fakeResetter{}, &fakeResetter{}
	r := NewRegistry(a)
	r.Register(b)
	r.Register(c)
	r.Register(nil) // no-op

	if got := r.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}

	r.ResetStaleProbes()
	r.ResetStaleProbes()

	for i, f := range []*fakeResetter{a, b, c} {
		if f.count != 2 {
			t.Errorf("breaker %d called %d times, want 2", i, f.count)
		}
	}
}

// TestRegistry_EmptyResetIsNoOp verifies that ResetStaleProbes on an empty
// registry does not panic.
func TestRegistry_EmptyResetIsNoOp(t *testing.T) {
	r := NewRegistry()
	r.ResetStaleProbes() // must not panic
	if r.Len() != 0 {
		t.Fatalf("Len = %d, want 0", r.Len())
	}
}

// TestRegistry_RealCircuitBreakerSatisfiesInterface verifies that a real
// *CircuitBreaker can be registered without type assertions.
func TestRegistry_RealCircuitBreakerSatisfiesInterface(t *testing.T) {
	cb := NewCircuitBreaker("test", 1, 0, func(error) bool { return true }, ErrBackendUnavailable)
	r := NewRegistry(cb)
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
	r.ResetStaleProbes() // must not panic on a closed circuit
}

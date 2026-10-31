// -------------------------------------------------------------------------------
// Breaker Registry Tests
//
// Author: Alex Freidah
//
// Verifies the registry the watchdog probes on each tick. Tests cover probing
// every breaker, a safe no-op when the registry is empty, and that the real
// *RecoveryProber satisfies Prober without an adapter.
// -------------------------------------------------------------------------------

package breaker

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
)

// fakeProber counts Probe calls; atomic because ProbeAll runs them
// concurrently.
type fakeProber struct {
	count    atomic.Int32
	workload atomic.Value
}

// Probe records that the registry probed this breaker, and the workload the
// probe would be charged to.
func (f *fakeProber) Probe(ctx context.Context) {
	f.count.Add(1)
	f.workload.Store(audit.WorkloadOf(ctx))
}

// TestRegistry_ProbeAll verifies that ProbeAll probes every breaker exactly
// once per call.
func TestRegistry_ProbeAll(t *testing.T) {
	a, b, c := &fakeProber{}, &fakeProber{}, &fakeProber{}
	r := NewRegistry(a, b, c)

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
	NewRegistry().ProbeAll(t.Context()) // must not panic
}

// TestRegistry_RecoveryProberSatisfiesInterface verifies that a real
// *RecoveryProber can be registered without type assertions.
func TestRegistry_RecoveryProberSatisfiesInterface(t *testing.T) {
	cb := NewCircuitBreaker(Config{Name: "test", Threshold: 1, Timeout: 0, IsError: func(error) bool { return true }, Sentinel: ErrBackendUnavailable})
	NewRegistry(NewRecoveryProber(cb, Recovery{Check: func(context.Context) error { return nil }})).
		ProbeAll(t.Context()) // must not panic on a closed circuit
}

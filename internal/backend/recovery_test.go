// -------------------------------------------------------------------------------
// RecoveryProber Tests
//
// Author: Alex Freidah
//
// Drives the prober with a fake clock: no check while closed, the first check
// after the open timeout, doubling backoff up to the cap while the backend
// stays down, recovery on success, and usage admission and charging.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
)

// fakeUsage admits health checks unless refuse is set and counts charges.
type fakeUsage struct {
	mu      sync.Mutex
	refuse  bool
	charged []s3op.Operation
}

// WithinLimits reports whether the fake is admitting calls.
func (f *fakeUsage) WithinLimits(string, []s3op.Operation, int64, int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.refuse
}

// Record logs the charged operation.
func (f *fakeUsage) Record(_ string, op s3op.Operation, _, _ int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.charged = append(f.charged, op)
}

// charges returns how many health checks were charged.
func (f *fakeUsage) charges() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.charged)
}

// proberFixture is an open breaker over a backend whose health check fails,
// with a prober on a fake clock starting at t0.
type proberFixture struct {
	mock   *mockBackend
	cb     *CircuitBreakerBackend
	usage  *fakeUsage
	prober *RecoveryProber
	now    time.Time
}

// newProberFixture opens a breaker with the given open timeout and builds its
// prober.
func newProberFixture(t *testing.T, timeout time.Duration) *proberFixture {
	t.Helper()
	mock := newMockBackend()
	mock.getErr = errors.New("connection refused")
	mock.bucketErr = errors.New("connection refused")
	cb := newTestCBBackend(mock, 1, timeout)
	_, _ = cb.GetObject(context.Background(), "key", "")
	if cb.State() != breaker.StateOpen {
		t.Fatalf("fixture breaker state = %v, want open", cb.State())
	}
	f := &proberFixture{mock: mock, cb: cb, usage: &fakeUsage{}, now: time.Unix(0, 0)}
	f.prober = NewRecoveryProber(cb, f.usage)
	f.prober.now = func() time.Time { return f.now }
	return f
}

// probeAt advances the fake clock by d and probes once.
func (f *proberFixture) probeAt(d time.Duration) {
	f.now = f.now.Add(d)
	f.prober.Probe(context.Background())
}

// setHealthy makes the backend's health check pass.
func (f *proberFixture) setHealthy() {
	f.mock.mu.Lock()
	f.mock.bucketErr = nil
	f.mock.mu.Unlock()
}

// TestRecoveryProber_ClosedDoesNothing verifies no health check runs while
// the circuit is closed.
func TestRecoveryProber_ClosedDoesNothing(t *testing.T) {
	t.Parallel()
	usage := &fakeUsage{}
	p := NewRecoveryProber(newTestCBBackend(newMockBackend(), 3, time.Minute), usage)
	for range 5 {
		p.Probe(context.Background())
	}
	if usage.charges() != 0 {
		t.Errorf("charged %d health checks on a closed circuit, want 0", usage.charges())
	}
}

// TestRecoveryProber_RecoversAfterOpenTimeout verifies the first check waits
// out the open timeout and a passing check closes the circuit.
func TestRecoveryProber_RecoversAfterOpenTimeout(t *testing.T) {
	t.Parallel()
	f := newProberFixture(t, time.Minute)
	f.setHealthy()

	f.probeAt(0)
	f.probeAt(59 * time.Second)
	if f.usage.charges() != 0 {
		t.Fatalf("checked %d times before the open timeout, want 0", f.usage.charges())
	}

	f.probeAt(time.Second)
	if f.cb.State() != breaker.StateClosed {
		t.Fatalf("state after a passing check = %v, want closed", f.cb.State())
	}
	if f.usage.charges() != 1 || f.usage.charged[0] != s3op.HeadBucket {
		t.Errorf("charged %v, want one HeadBucket", f.usage.charged)
	}
}

// TestRecoveryProber_BacksOffToCap verifies failed checks double the interval
// until it reaches maxRecoveryBackoff.
func TestRecoveryProber_BacksOffToCap(t *testing.T) {
	t.Parallel()
	f := newProberFixture(t, time.Minute)
	f.probeAt(0)

	for i, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		f.probeAt(wait - time.Second)
		if f.usage.charges() != i {
			t.Fatalf("check %d ran %v early", i+1, time.Second)
		}
		f.probeAt(time.Second)
		if f.usage.charges() != i+1 {
			t.Fatalf("check %d did not run after %v", i+1, wait)
		}
	}
	if f.cb.State() != breaker.StateOpen {
		t.Errorf("state while the backend is down = %v, want open", f.cb.State())
	}
}

// TestRecoveryProber_LongOpenTimeoutIsTheCap verifies an open timeout longer
// than maxRecoveryBackoff is not shortened by the backoff.
func TestRecoveryProber_LongOpenTimeoutIsTheCap(t *testing.T) {
	t.Parallel()
	f := newProberFixture(t, 10*time.Minute)
	f.probeAt(0)
	f.probeAt(10 * time.Minute)
	f.probeAt(10*time.Minute - time.Second)
	if f.usage.charges() != 1 {
		t.Fatalf("checks = %d, want 1 before the second full open timeout", f.usage.charges())
	}
	f.probeAt(time.Second)
	if f.usage.charges() != 2 {
		t.Fatalf("checks = %d, want 2", f.usage.charges())
	}
}

// TestRecoveryProber_OverLimitSkips verifies a check the usage budget would
// refuse is neither run nor charged, and runs as soon as the budget allows.
func TestRecoveryProber_OverLimitSkips(t *testing.T) {
	t.Parallel()
	f := newProberFixture(t, time.Minute)
	f.setHealthy()
	f.usage.refuse = true

	f.probeAt(0)
	f.probeAt(time.Hour)
	if f.usage.charges() != 0 || f.cb.State() != breaker.StateOpen {
		t.Fatalf("over limit: charges = %d, state = %v; want 0 and open", f.usage.charges(), f.cb.State())
	}

	f.usage.mu.Lock()
	f.usage.refuse = false
	f.usage.mu.Unlock()
	f.probeAt(breaker.DefaultWatchdogInterval)
	if f.cb.State() != breaker.StateClosed {
		t.Errorf("state once the budget allows = %v, want closed", f.cb.State())
	}
}

// TestRecoveryProber_ScheduleResetsAfterRecovery verifies a breaker that
// reopens after recovering starts again from the open timeout, not from the
// backoff it had reached.
func TestRecoveryProber_ScheduleResetsAfterRecovery(t *testing.T) {
	t.Parallel()
	f := newProberFixture(t, time.Minute)
	f.probeAt(0)
	f.probeAt(time.Minute)
	f.probeAt(2 * time.Minute)
	f.setHealthy()
	f.probeAt(4 * time.Minute)
	if f.cb.State() != breaker.StateClosed {
		t.Fatalf("state = %v, want closed", f.cb.State())
	}

	_, _ = f.cb.GetObject(context.Background(), "key", "")
	f.probeAt(0)
	f.probeAt(time.Minute)
	if f.cb.State() != breaker.StateClosed {
		t.Errorf("state one open timeout after reopening = %v, want closed", f.cb.State())
	}
}

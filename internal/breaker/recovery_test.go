// -------------------------------------------------------------------------------
// RecoveryProber Tests
//
// Author: Alex Freidah
//
// Drives the prober with a fake clock: no check while closed, the first check
// after the open timeout, doubling backoff up to the cap while the dependency
// stays down, no backoff without a cap, recovery on success, and admission.
// -------------------------------------------------------------------------------

package breaker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// proberFixture is an open breaker whose dependency check fails until
// healthy is set, with a prober on a fake clock starting at t0.
type proberFixture struct {
	breaker *CircuitBreaker
	prober  *RecoveryProber
	healthy atomic.Bool
	refuse  atomic.Bool
	checks  atomic.Int32
	now     time.Time
}

// newProberFixture opens a breaker with the given open timeout and builds its
// prober, capping the backoff at maxBackoff.
func newProberFixture(t *testing.T, timeout, maxBackoff time.Duration) *proberFixture {
	t.Helper()
	f := &proberFixture{breaker: newTestBreaker(1, timeout), now: time.Unix(0, 0)}
	_ = f.breaker.PostCheck(errTest)
	f.prober = NewRecoveryProber(f.breaker, Recovery{
		Check: func(context.Context) error {
			f.checks.Add(1)
			if f.healthy.Load() {
				return nil
			}
			return errors.New("connection refused")
		},
		MaxBackoff: maxBackoff,
		Admit:      func(context.Context) bool { return !f.refuse.Load() },
	})
	f.prober.now = func() time.Time { return f.now }
	return f
}

// probeAt advances the fake clock by d and probes once.
func (f *proberFixture) probeAt(d time.Duration) {
	f.now = f.now.Add(d)
	f.prober.Probe(context.Background())
}

// TestRecoveryProber_ClosedDoesNothing verifies no check runs while the
// circuit is closed.
func TestRecoveryProber_ClosedDoesNothing(t *testing.T) {
	t.Parallel()
	f := newProberFixture(t, time.Minute, 0)
	f.breaker.Recover()
	for range 5 {
		f.probeAt(time.Hour)
	}
	if f.checks.Load() != 0 {
		t.Errorf("ran %d checks on a closed circuit, want 0", f.checks.Load())
	}
}

// TestRecoveryProber_RecoversAfterOpenTimeout verifies the first check waits
// out the open timeout and a passing check closes the circuit.
func TestRecoveryProber_RecoversAfterOpenTimeout(t *testing.T) {
	t.Parallel()
	f := newProberFixture(t, time.Minute, 0)
	f.healthy.Store(true)

	f.probeAt(0)
	f.probeAt(59 * time.Second)
	if f.checks.Load() != 0 {
		t.Fatalf("checked %d times before the open timeout, want 0", f.checks.Load())
	}

	f.probeAt(time.Second)
	if f.breaker.State() != StateClosed || f.checks.Load() != 1 {
		t.Errorf("after one passing check: state = %v, checks = %d; want closed and 1", f.breaker.State(), f.checks.Load())
	}
}

// TestRecoveryProber_BacksOffToCap verifies failed checks double the interval
// until it reaches the cap.
func TestRecoveryProber_BacksOffToCap(t *testing.T) {
	t.Parallel()
	f := newProberFixture(t, time.Minute, 5*time.Minute)
	f.probeAt(0)

	for i, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		f.probeAt(wait - time.Second)
		if int(f.checks.Load()) != i {
			t.Fatalf("check %d ran %v early", i+1, time.Second)
		}
		f.probeAt(time.Second)
		if int(f.checks.Load()) != i+1 {
			t.Fatalf("check %d did not run after %v", i+1, wait)
		}
	}
	if f.breaker.State() != StateOpen {
		t.Errorf("state while the dependency is down = %v, want open", f.breaker.State())
	}
}

// TestRecoveryProber_NoCapChecksEveryOpenTimeout verifies a recovery with no
// backoff cap, as the database's free ping uses, checks once per open timeout
// for as long as the dependency stays down.
func TestRecoveryProber_NoCapChecksEveryOpenTimeout(t *testing.T) {
	t.Parallel()
	f := newProberFixture(t, 15*time.Second, 0)
	f.probeAt(0)
	for i := range 4 {
		f.probeAt(15 * time.Second)
		if int(f.checks.Load()) != i+1 {
			t.Fatalf("checks after %d open timeouts = %d, want %d", i+1, f.checks.Load(), i+1)
		}
	}
}

// TestRecoveryProber_LongOpenTimeoutIsTheCap verifies an open timeout longer
// than the cap is not shortened by the backoff.
func TestRecoveryProber_LongOpenTimeoutIsTheCap(t *testing.T) {
	t.Parallel()
	f := newProberFixture(t, 10*time.Minute, 5*time.Minute)
	f.probeAt(0)
	f.probeAt(10 * time.Minute)
	f.probeAt(10*time.Minute - time.Second)
	if f.checks.Load() != 1 {
		t.Fatalf("checks = %d, want 1 before the second full open timeout", f.checks.Load())
	}
	f.probeAt(time.Second)
	if f.checks.Load() != 2 {
		t.Fatalf("checks = %d, want 2", f.checks.Load())
	}
}

// TestRecoveryProber_RefusedAdmissionSkips verifies a check admission refuses
// is not run, and runs as soon as it is admitted.
func TestRecoveryProber_RefusedAdmissionSkips(t *testing.T) {
	t.Parallel()
	f := newProberFixture(t, time.Minute, 0)
	f.healthy.Store(true)
	f.refuse.Store(true)

	f.probeAt(0)
	f.probeAt(time.Hour)
	if f.checks.Load() != 0 || f.breaker.State() != StateOpen {
		t.Fatalf("refused: checks = %d, state = %v; want 0 and open", f.checks.Load(), f.breaker.State())
	}

	f.refuse.Store(false)
	f.probeAt(DefaultWatchdogInterval)
	if f.breaker.State() != StateClosed {
		t.Errorf("state once admitted = %v, want closed", f.breaker.State())
	}
}

// TestRecoveryProber_ScheduleResetsAfterRecovery verifies a breaker that
// reopens after recovering starts again from the open timeout, not from the
// backoff it had reached.
func TestRecoveryProber_ScheduleResetsAfterRecovery(t *testing.T) {
	t.Parallel()
	f := newProberFixture(t, time.Minute, 5*time.Minute)
	f.probeAt(0)
	f.probeAt(time.Minute)
	f.probeAt(2 * time.Minute)
	f.healthy.Store(true)
	f.probeAt(4 * time.Minute)
	if f.breaker.State() != StateClosed {
		t.Fatalf("state = %v, want closed", f.breaker.State())
	}

	_ = f.breaker.PostCheck(errTest)
	f.probeAt(0)
	f.probeAt(time.Minute)
	if f.breaker.State() != StateClosed {
		t.Errorf("state one open timeout after reopening = %v, want closed", f.breaker.State())
	}
}

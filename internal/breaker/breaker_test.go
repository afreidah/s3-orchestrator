// -------------------------------------------------------------------------------
// Circuit Breaker Core Tests
//
// Author: Alex Freidah
//
// Tests for the generic CircuitBreaker state machine: state transitions,
// pluggable error filter, recovery only through Recover, and call helpers.
// -------------------------------------------------------------------------------

package breaker

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// alwaysError is an error filter that treats all non-nil errors as failures.
func alwaysError(err error) bool { return err != nil }

// neverError is an error filter that never treats errors as failures.
func neverError(_ error) bool { return false }

// errTest is the non-DB sentinel passed through in tests where
// the call result must NOT trip the breaker.
var errTest = errors.New("test error")

// errSentinel is the DB-typed sentinel passed through in tests
// where the call result must trip the breaker.
var errSentinel = errors.New("circuit open")

// newTestBreaker constructs a new test breaker.
func newTestBreaker(threshold int, timeout time.Duration) *CircuitBreaker {
	return NewCircuitBreaker(Config{Name: "test", Threshold: threshold, Timeout: timeout, IsError: alwaysError, Sentinel: errSentinel})
}

// -------------------------------------------------------------------------
// State transitions
// -------------------------------------------------------------------------

// TestCB_StartsHealthy verifies the cb starts healthy contract.
// Asserts that expected closed, got.
func TestCB_StartsHealthy(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(3, time.Minute)
	if !cb.IsHealthy() {
		t.Fatal("should start healthy")
	}
	if cb.State() != StateClosed {
		t.Fatalf("expected closed, got %v", cb.State())
	}
}

// TestCB_OpensAfterThreshold verifies the cb opens after threshold contract.
// Asserts that call : should return raw error below threshold.
func TestCB_OpensAfterThreshold(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(3, time.Minute)

	for i := range 2 {
		err := cb.PostCheck(errTest)
		if errors.Is(err, errSentinel) {
			t.Fatalf("call %d: should return raw error below threshold", i)
		}
	}
	if !cb.IsHealthy() {
		t.Fatal("should still be healthy below threshold")
	}

	// 3rd failure trips the breaker
	err := cb.PostCheck(errTest)
	if !errors.Is(err, errSentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}
	if cb.IsHealthy() {
		t.Fatal("should be unhealthy after tripping")
	}
}

// TestCB_OpenRejectsCalls verifies the cb open rejects calls contract.
// Asserts that expected sentinel, got.
func TestCB_OpenRejectsCalls(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(1, time.Minute)
	_ = cb.PostCheck(errTest) // trip

	if err := cb.PreCheck(); !errors.Is(err, errSentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}
}

// TestCB_OpenStaysOpenPastTimeout verifies an open breaker keeps refusing
// calls after its open timeout, since no call is let through to find out
// whether the dependency is back, and closes only through Recover.
func TestCB_OpenStaysOpenPastTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, 5*time.Millisecond)
		_ = cb.PostCheck(errTest)
		time.Sleep(time.Second)

		if err := cb.PreCheck(); !errors.Is(err, errSentinel) {
			t.Fatalf("PreCheck past the open timeout = %v, want the sentinel", err)
		}
		cb.Recover()
		if err := cb.PreCheck(); err != nil {
			t.Fatalf("PreCheck after Recover = %v, want nil", err)
		}
	})
}

// TestCB_TripOpensBeforeTheThreshold verifies Trip opens a closed breaker on
// its own, for an owner that already knows the dependency is down.
func TestCB_TripOpensBeforeTheThreshold(t *testing.T) {
	cb := newTestBreaker(5, time.Hour)
	cb.Trip(errTest)
	if err := cb.PreCheck(); !errors.Is(err, errSentinel) {
		t.Fatalf("PreCheck after Trip = %v, want the sentinel", err)
	}
}

// TestCB_SuccessWhileOpenDoesNotClose verifies a call that succeeds while the
// circuit is open, one that went around PreCheck as the Redis hot path does,
// leaves the circuit open: only Recover closes it.
func TestCB_SuccessWhileOpenDoesNotClose(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(1, time.Minute)
	_ = cb.PostCheck(errTest)
	_ = cb.PostCheck(nil)
	if cb.State() != StateOpen {
		t.Errorf("state after a success while open = %v, want open", cb.State())
	}
}

// TestCB_SuccessResetsFailureCount verifies the cb success resets failure count path by exercising cb.PostCheck, cb.IsHealthy.
func TestCB_SuccessResetsFailureCount(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(3, time.Minute)

	_ = cb.PostCheck(errTest) // 1
	_ = cb.PostCheck(errTest) // 2
	_ = cb.PostCheck(nil)     // success resets
	_ = cb.PostCheck(errTest) // 1 again
	_ = cb.PostCheck(errTest) // 2 again

	if !cb.IsHealthy() {
		t.Fatal("should still be healthy  -  counter was reset")
	}
}

// -------------------------------------------------------------------------
// OpenDuration
// -------------------------------------------------------------------------

// TestCB_OpenDuration_ZeroWhenClosed verifies the cb open duration zero when closed contract.
// Asserts that expected 0 when closed, got.
func TestCB_OpenDuration_ZeroWhenClosed(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(3, time.Minute)
	if d := cb.OpenDuration(); d != 0 {
		t.Fatalf("expected 0 when closed, got %v", d)
	}
}

// TestCB_OpenDuration_PositiveWhenOpen verifies the cb open duration positive when open contract.
// Asserts that expected >= 5ms, got.
func TestCB_OpenDuration_PositiveWhenOpen(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, time.Minute)
		_ = cb.PostCheck(errTest) // trip

		time.Sleep(5 * time.Millisecond)
		d := cb.OpenDuration()
		if d < 5*time.Millisecond {
			t.Fatalf("expected >= 5ms, got %v", d)
		}
	})
}

// TestCB_OpenDuration_ZeroAfterRecovery verifies the cb open duration zero after recovery contract.
// Asserts that expected 0 after recovery, got.
func TestCB_OpenDuration_ZeroAfterRecovery(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, 10*time.Millisecond)
		_ = cb.PostCheck(errTest) // trip
		time.Sleep(15 * time.Millisecond)
		cb.Recover()

		if d := cb.OpenDuration(); d != 0 {
			t.Fatalf("expected 0 after recovery, got %v", d)
		}
	})
}

// -------------------------------------------------------------------------
// Pluggable error filter
// -------------------------------------------------------------------------

// TestCB_FilteredErrorsDontTrip verifies the cb filtered errors dont trip path by exercising cb.PostCheck, cb.IsHealthy.
func TestCB_FilteredErrorsDontTrip(t *testing.T) {
	t.Parallel()
	cb := NewCircuitBreaker(Config{Name: "test", Threshold: 1, Timeout: time.Minute, IsError: neverError, Sentinel: errSentinel})

	for range 10 {
		_ = cb.PostCheck(errTest)
	}
	if !cb.IsHealthy() {
		t.Fatal("filtered errors should not trip the breaker")
	}
}

// TestCB_NilErrorIsSuccess verifies the cb nil error is success contract.
// Asserts that nil error should pass through as nil, got.
func TestCB_NilErrorIsSuccess(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(1, time.Minute)
	err := cb.PostCheck(nil)
	if err != nil {
		t.Fatalf("nil error should pass through as nil, got %v", err)
	}
	if !cb.IsHealthy() {
		t.Fatal("nil error should not trip")
	}
}

// -------------------------------------------------------------------------
// Call helper
// -------------------------------------------------------------------------

// TestCBCall_PassesThrough verifies the cbcall passes through contract.
// Asserts that unexpected: result= err=.
func TestCBCall_PassesThrough(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(3, time.Minute)
	result, err := cb.Call(func() (string, error) { return "ok", nil })
	if err != nil || result != "ok" {
		t.Fatalf("unexpected: result=%q err=%v", result, err)
	}
}

// TestCBCall_CircuitOpen verifies the cbcall circuit open contract.
// Asserts that expected sentinel, got.
func TestCBCall_CircuitOpen(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(1, time.Minute)
	_ = cb.PostCheck(errTest) // trip

	called := false
	_, err := cb.Call(func() (string, error) {
		called = true
		return "ok", nil
	})
	if !errors.Is(err, errSentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}
	if called {
		t.Fatal("function should not be called when circuit is open")
	}
}

// TestCB_OpeningErrorWrapsCause verifies the error returned when a failure
// opens the circuit matches the sentinel and still wraps the failure.
func TestCB_OpeningErrorWrapsCause(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(1, time.Minute)
	err := cb.PostCheck(errTest)
	if !errors.Is(err, errSentinel) || !errors.Is(err, errTest) {
		t.Errorf("PostCheck = %v, want it to match both the sentinel and the cause", err)
	}
}

// TestTransition_InvokesHookOnOpen verifies the OnStateChange callback is
// fired for the closed->open transition with the failure context populated.
func TestTransition_InvokesHookOnOpen(t *testing.T) {
	var infos []StateChangeInfo
	cb := newTestBreaker(2, time.Hour)
	cb.AddOnStateChange(func(i StateChangeInfo) { infos = append(infos, i) })

	_ = cb.PostCheck(errTest)
	_ = cb.PostCheck(errTest)

	if len(infos) != 1 {
		t.Fatalf("expected 1 hook invocation, got %d", len(infos))
	}
	got := infos[0]
	if got.From != StateClosed || got.To != StateOpen {
		t.Errorf("transition = %s->%s, want closed->open", got.From, got.To)
	}
	if got.Failures < 2 || got.Threshold != 2 || got.Name != "test" {
		t.Errorf("info = %+v", got)
	}
}

// TestTransition_InvokesHookOnRecovery walks the open -> closed cycle and
// confirms the hook fires for each transition with a non-zero OpenDuration on
// the recovery edge.
func TestTransition_InvokesHookOnRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var infos []StateChangeInfo
		cb := newTestBreaker(1, 10*time.Millisecond)
		cb.AddOnStateChange(func(i StateChangeInfo) { infos = append(infos, i) })

		_ = cb.PostCheck(errTest) // closed -> open
		time.Sleep(15 * time.Millisecond)
		cb.Recover() // open -> closed

		if len(infos) != 2 {
			t.Fatalf("expected 2 hook invocations, got %d", len(infos))
		}
		closed := infos[1]
		if closed.From != StateOpen || closed.To != StateClosed {
			t.Errorf("final transition = %s->%s, want open->closed", closed.From, closed.To)
		}
		if closed.OpenDuration <= 0 {
			t.Error("OpenDuration should be > 0 on recovery")
		}
	})
}

// TestTransition_NilHookIsNoop verifies the breaker still transitions
// correctly when no OnStateChange callback is installed.
func TestTransition_NilHookIsNoop(t *testing.T) {
	cb := newTestBreaker(1, time.Hour)
	_ = cb.PostCheck(errTest)

	if cb.State() != StateOpen {
		t.Errorf("state = %s, want open", cb.State())
	}
}

// TestAddOnStateChange_ChainsListeners verifies that AddOnStateChange
// appends to the existing hook rather than replacing it; both the
// telemetry listener and the recovery listener must fire on transitions.
func TestAddOnStateChange_ChainsListeners(t *testing.T) {
	var standardCalls, addedCalls int
	cb := newTestBreaker(1, time.Hour)
	cb.AddOnStateChange(func(StateChangeInfo) { standardCalls++ })
	cb.AddOnStateChange(func(StateChangeInfo) { addedCalls++ })

	_ = cb.PostCheck(errTest) // closed -> open

	if standardCalls != 1 || addedCalls != 1 {
		t.Errorf("standard=%d added=%d, want 1/1", standardCalls, addedCalls)
	}
}

// TestAddOnStateChange_NoPriorHook verifies AddOnStateChange installs
// the first callback on a breaker that has none.
func TestAddOnStateChange_NoPriorHook(t *testing.T) {
	var calls int
	cb := newTestBreaker(1, time.Hour)
	cb.AddOnStateChange(func(StateChangeInfo) { calls++ })

	_ = cb.PostCheck(errTest)

	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// -------------------------------------------------------------------------
// Recover (out-of-band recovery probe)
// -------------------------------------------------------------------------

// TestCB_RecoverFromOpen verifies Recover moves an open breaker straight to
// closed without going through PreCheck, which out-of-band probes such as the
// Redis ping rely on.
func TestCB_RecoverFromOpen(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(3, time.Hour)

	for range 3 {
		_ = cb.PostCheck(errTest)
	}
	if cb.IsHealthy() {
		t.Fatal("setup: breaker should be open after threshold failures")
	}
	if cb.State() != StateOpen {
		t.Fatalf("setup: state = %s, want open", cb.State())
	}

	cb.Recover()

	if !cb.IsHealthy() {
		t.Errorf("breaker still unhealthy after Recover()")
	}
	if cb.State() != StateClosed {
		t.Errorf("state = %s, want closed", cb.State())
	}
}

// TestCB_RecoverFromClosed asserts Recover() is a safe no-op on an
// already-closed breaker (idempotent).
func TestCB_RecoverFromClosed(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(3, time.Hour)
	if !cb.IsHealthy() {
		t.Fatal("setup: should start closed")
	}

	cb.Recover()

	if !cb.IsHealthy() {
		t.Errorf("breaker became unhealthy after Recover() on a closed breaker")
	}
	if cb.State() != StateClosed {
		t.Errorf("state = %s, want closed", cb.State())
	}
}

// TestCB_RecoverResetsFailureCounter pins the contract that Recover()
// zeroes the failure counter so the breaker tolerates the configured
// threshold of new failures before tripping again. Without this, the
// next failure post-recovery would immediately re-open the breaker
// since Recover-without-reset would leave failures = threshold.
func TestCB_RecoverResetsFailureCounter(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(3, time.Hour)
	for range 3 {
		_ = cb.PostCheck(errTest)
	}
	if cb.IsHealthy() {
		t.Fatal("setup: breaker should be open")
	}

	cb.Recover()

	// One failure post-recovery must NOT immediately re-open the
	// breaker; the failure counter must have been zeroed.
	_ = cb.PostCheck(errTest)
	if !cb.IsHealthy() {
		t.Errorf("a single failure post-recovery re-opened the breaker; failure counter not reset")
	}
}

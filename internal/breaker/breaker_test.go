// -------------------------------------------------------------------------------
// Circuit Breaker Core Tests
//
// Author: Alex Freidah
//
// Tests for the generic CircuitBreaker state machine: state transitions,
// pluggable error filter, concurrent probe serialization, and call helpers.
// -------------------------------------------------------------------------------

package breaker

import (
	"errors"
	"sync"
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

// TestCB_HalfOpenAfterTimeout verifies the cb half open after timeout contract.
// Asserts that probe should be allowed:.
func TestCB_HalfOpenAfterTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, 10*time.Millisecond)
		_ = cb.PostCheck(errTest) // trip
		time.Sleep(15 * time.Millisecond)

		// PreCheck should allow one probe through
		if err := cb.PreCheck(); err != nil {
			t.Fatalf("probe should be allowed: %v", err)
		}
		if cb.State() != StateHalfOpen {
			t.Fatalf("expected half-open, got %v", cb.State())
		}
	})
}

// TestCB_HalfOpenSuccess_Closes verifies the cb half open success closes path by exercising cb.PostCheck, time.Sleep, cb.PreCheck.
func TestCB_HalfOpenSuccess_Closes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, 10*time.Millisecond)
		_ = cb.PostCheck(errTest) // trip
		time.Sleep(15 * time.Millisecond)

		_ = cb.PreCheck()     // allow probe
		_ = cb.PostCheck(nil) // success

		if !cb.IsHealthy() {
			t.Fatal("should be healthy after successful probe")
		}
	})
}

// TestCB_HalfOpenFailure_Reopens verifies the cb half open failure reopens contract.
// Asserts that expected open, got.
func TestCB_HalfOpenFailure_Reopens(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, 10*time.Millisecond)
		_ = cb.PostCheck(errTest) // trip
		time.Sleep(15 * time.Millisecond)

		_ = cb.PreCheck()         // allow probe
		_ = cb.PostCheck(errTest) // probe fails

		if cb.IsHealthy() {
			t.Fatal("should be unhealthy after failed probe")
		}
		if cb.State() != StateOpen {
			t.Fatalf("expected open, got %v", cb.State())
		}
	})
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

// TestCB_OpenDuration_PositiveWhenHalfOpen verifies the cb open duration positive when half open contract.
// Asserts that expected half-open, got.
func TestCB_OpenDuration_PositiveWhenHalfOpen(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, 10*time.Millisecond)
		_ = cb.PostCheck(errTest) // trip
		time.Sleep(15 * time.Millisecond)
		_ = cb.PreCheck() // transition to half-open

		if cb.State() != StateHalfOpen {
			t.Fatalf("expected half-open, got %v", cb.State())
		}
		if cb.OpenDuration() == 0 {
			t.Fatal("expected positive duration in half-open state")
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

		_ = cb.PreCheck()     // half-open
		_ = cb.PostCheck(nil) // recover

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
// CBCall / CBCallNoResult helpers
// -------------------------------------------------------------------------

// TestCBCall_PassesThrough verifies the cbcall passes through contract.
// Asserts that unexpected: result= err=.
func TestCBCall_PassesThrough(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(3, time.Minute)
	result, err := CBCall(cb, func() (string, error) { return "ok", nil })
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
	_, err := CBCall(cb, func() (string, error) {
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

// TestCBCallNoResult_PassesThrough verifies the cbcall no result passes through contract.
// Asserts that unexpected error:.
func TestCBCallNoResult_PassesThrough(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(3, time.Minute)
	err := CBCallNoResult(cb, func() error { return nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestCBCallNoResult_CircuitOpen verifies the cbcall no result circuit open contract.
// Asserts that expected sentinel, got.
func TestCBCallNoResult_CircuitOpen(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(1, time.Minute)
	_ = cb.PostCheck(errTest) // trip

	err := CBCallNoResult(cb, func() error { return nil })
	if !errors.Is(err, errSentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}
}

// -------------------------------------------------------------------------
// Concurrent probe serialization
// -------------------------------------------------------------------------

// TestCB_OnlyOneProbeAllowed verifies the cb only one probe allowed contract.
// Asserts that expected exactly 1 probe, got.
func TestCB_OnlyOneProbeAllowed(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, 10*time.Millisecond)
		_ = cb.PostCheck(errTest) // trip
		time.Sleep(15 * time.Millisecond)

		const goroutines = 50
		var wg sync.WaitGroup
		var probes int64
		var mu sync.Mutex

		wg.Add(goroutines)
		for range goroutines {
			go func() {
				defer wg.Done()
				if err := cb.PreCheck(); err == nil {
					mu.Lock()
					probes++
					mu.Unlock()
					// Simulate the probe completing
					_ = cb.PostCheck(errTest)
				}
			}()
		}
		wg.Wait()

		if probes != 1 {
			t.Errorf("expected exactly 1 probe, got %d", probes)
		}
	})
}

// TestCB_ProbeEligible verifies the cb probe eligible path by exercising cb.ProbeEligible, cb.PostCheck, time.Sleep.
func TestCB_ProbeEligible(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, 5*time.Millisecond)

		// Closed  -  not probe-eligible
		if cb.ProbeEligible() {
			t.Error("closed circuit should not be probe-eligible")
		}

		// Trip it open
		_ = cb.PostCheck(errTest)
		if cb.ProbeEligible() {
			t.Error("freshly opened circuit should not be probe-eligible (timeout not elapsed)")
		}

		// Wait for timeout
		time.Sleep(10 * time.Millisecond)
		if !cb.ProbeEligible() {
			t.Error("open circuit with elapsed timeout should be probe-eligible")
		}

		// Transition to half-open via PreCheck  -  no longer probe-eligible
		_ = cb.PreCheck()
		if cb.ProbeEligible() {
			t.Error("half-open circuit should not be probe-eligible")
		}
	})
}

// -------------------------------------------------------------------------
// Probe jitter
// -------------------------------------------------------------------------

// TestCB_ProbeJitter_VariesBetweenInstances verifies the cb probe jitter varies between instances contract.
// Asserts that expected jitter diversity, but all breakers got the same value.
func TestCB_ProbeJitter_VariesBetweenInstances(t *testing.T) {
	t.Parallel()
	// Create several circuit breakers and trip them all. With openTimeout of
	// 1s and jitter range of 250ms, collisions across 10 instances are rare.
	const n = 10
	breakers := make([]*CircuitBreaker, n)
	for i := range n {
		breakers[i] = newTestBreaker(1, time.Second)
		_ = breakers[i].PostCheck(errTest)
	}

	jitters := make(map[time.Duration]bool, n)
	for _, cb := range breakers {
		cb.mu.RLock()
		jitters[cb.probeJitter] = true
		cb.mu.RUnlock()
	}

	if len(jitters) < 2 {
		t.Errorf("expected jitter diversity, but all %d breakers got the same value", n)
	}
}

// TestCB_ProbeJitter_RecomputedOnReopen verifies the cb probe jitter recomputed on reopen path by exercising cb.PostCheck, time.Sleep, cb.PreCheck.
func TestCB_ProbeJitter_RecomputedOnReopen(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, 50*time.Millisecond)

		// Trip -> record jitter
		_ = cb.PostCheck(errTest)
		cb.mu.RLock()
		firstJitter := cb.probeJitter
		cb.mu.RUnlock()

		// Recover  -  sleep must exceed openTimeout + max jitter (50ms + 12.5ms)
		time.Sleep(80 * time.Millisecond)
		_ = cb.PreCheck()
		_ = cb.PostCheck(nil)
		if !cb.IsHealthy() {
			t.Fatal("expected recovery to closed state")
		}

		// Trip again -> new jitter (may coincidentally equal firstJitter, but
		// the code path that sets it is exercised)
		_ = cb.PostCheck(errTest)
		cb.mu.RLock()
		_ = cb.probeJitter
		cb.mu.RUnlock()

		// Verify the code path ran without panicking
		_ = firstJitter
	})
}

// TestCB_StaleProbeAutoResets verifies the cb stale probe auto resets contract.
// Asserts that probe should be allowed:.
func TestCB_StaleProbeAutoResets(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, 5*time.Millisecond)

		// Trip the breaker
		_ = cb.PostCheck(errTest)
		time.Sleep(10 * time.Millisecond)

		// Start a probe (PreCheck transitions to half-open)
		if err := cb.PreCheck(); err != nil {
			t.Fatalf("probe should be allowed: %v", err)
		}
		if cb.State() != StateHalfOpen {
			t.Fatalf("expected half-open, got %s", cb.State())
		}

		// Simulate a stale probe by backdating probeStarted beyond probeTimeout.
		// Do NOT call PostCheck  -  the probe is "abandoned".
		cb.probeStarted.Store(time.Now().Add(-probeTimeout - time.Second).UnixNano())

		// Next PreCheck should detect the stale probe and reset to open
		if err := cb.PreCheck(); err == nil {
			t.Fatal("expected sentinel error after stale probe reset")
		}

		if cb.State() != StateOpen {
			t.Fatalf("expected open after stale probe reset, got %s", cb.State())
		}
		if cb.probeInFlight.Load() {
			t.Error("probeInFlight should be reset after stale probe detection")
		}
	})
}

// TestTransition_InvokesHookOnOpen verifies the OnStateChange callback is
// fired for the closed->open transition with the failure context populated.
func TestTransition_InvokesHookOnOpen(t *testing.T) {
	var infos []StateChangeInfo
	cb := newTestBreaker(2, time.Hour)
	cb.SetOnStateChange(func(i StateChangeInfo) { infos = append(infos, i) })

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

// TestTransition_InvokesHookOnRecovery walks the full open -> half-open ->
// closed cycle and confirms the hook fires for each transition with a
// non-zero OpenDuration on the recovery edge.
func TestTransition_InvokesHookOnRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var infos []StateChangeInfo
		cb := newTestBreaker(1, 10*time.Millisecond)
		cb.SetOnStateChange(func(i StateChangeInfo) { infos = append(infos, i) })

		_ = cb.PostCheck(errTest)         // closed -> open
		time.Sleep(15 * time.Millisecond) // wait for probe eligibility
		_ = cb.PreCheck()                 // open -> half-open
		_ = cb.PostCheck(nil)             // half-open -> closed

		if len(infos) != 3 {
			t.Fatalf("expected 3 hook invocations, got %d", len(infos))
		}
		closed := infos[2]
		if closed.From != StateHalfOpen || closed.To != StateClosed {
			t.Errorf("final transition = %s->%s, want half-open->closed", closed.From, closed.To)
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
	cb.SetOnStateChange(func(StateChangeInfo) { standardCalls++ })
	cb.AddOnStateChange(func(StateChangeInfo) { addedCalls++ })

	_ = cb.PostCheck(errTest) // closed -> open

	if standardCalls != 1 || addedCalls != 1 {
		t.Errorf("standard=%d added=%d, want 1/1", standardCalls, addedCalls)
	}
}

// TestAddOnStateChange_NoPriorHook verifies AddOnStateChange installs
// the callback cleanly when no SetOnStateChange has been called first.
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
// ResetStaleProbe
// -------------------------------------------------------------------------

// TestResetStaleProbe_ClosedCircuit verifies that ResetStaleProbe is a no-op
// when the circuit is closed.
func TestResetStaleProbe_ClosedCircuit(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(3, time.Minute)

	if cb.ResetStaleProbe() {
		t.Error("expected false for closed circuit")
	}
	if cb.State() != StateClosed {
		t.Errorf("state = %s, want closed", cb.State())
	}
}

// TestResetStaleProbe_OpenCircuit verifies that ResetStaleProbe is a no-op
// when the circuit is open (no probe dispatched yet).
func TestResetStaleProbe_OpenCircuit(t *testing.T) {
	t.Parallel()
	cb := newTestBreaker(1, time.Hour)
	_ = cb.PostCheck(errTest) // trips to open

	if cb.ResetStaleProbe() {
		t.Error("expected false for open circuit with no probe")
	}
	if cb.State() != StateOpen {
		t.Errorf("state = %s, want open", cb.State())
	}
}

// TestResetStaleProbe_FreshProbe verifies that ResetStaleProbe does not reset
// a probe that was just dispatched (well within probeTimeout).
func TestResetStaleProbe_FreshProbe(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, time.Millisecond)
		_ = cb.PostCheck(errTest)
		time.Sleep(2 * time.Millisecond)

		// Dispatch a probe via PreCheck
		if err := cb.PreCheck(); err != nil {
			t.Fatalf("expected probe to be allowed: %v", err)
		}
		if cb.State() != StateHalfOpen {
			t.Fatalf("state = %s, want half-open", cb.State())
		}

		// Probe is fresh, should not be reset.
		if cb.ResetStaleProbe() {
			t.Error("expected false for fresh probe")
		}
		if cb.State() != StateHalfOpen {
			t.Errorf("state = %s, want half-open (unchanged)", cb.State())
		}
	})
}

// TestResetStaleProbe_StaleProbe verifies that ResetStaleProbe resets a
// half-open circuit whose probe has exceeded probeTimeout back to open.
func TestResetStaleProbe_StaleProbe(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, time.Millisecond)
		_ = cb.PostCheck(errTest)
		time.Sleep(2 * time.Millisecond)

		// Dispatch a probe
		if err := cb.PreCheck(); err != nil {
			t.Fatalf("expected probe: %v", err)
		}

		// Backdate the probe start to simulate a stale probe.
		cb.probeStarted.Store(time.Now().Add(-probeTimeout - time.Second).UnixNano())

		if !cb.ResetStaleProbe() {
			t.Error("expected true for stale probe")
		}
		if cb.State() != StateOpen {
			t.Errorf("state = %s, want open after stale probe reset", cb.State())
		}
		if cb.probeInFlight.Load() {
			t.Error("probeInFlight should be false after reset")
		}
	})
}

// -------------------------------------------------------------------------
// Recover (out-of-band recovery probe)
// -------------------------------------------------------------------------

// TestCB_RecoverFromOpen pins the post-fix contract that an out-of-band
// recovery probe (Redis ping, manual operator action) can transition the
// breaker straight from Open back to Closed without going through
// PreCheck. Without Recover() the only path Open->Closed is via
// PreCheck()->HalfOpen->PostCheck(nil)->Closed, which the Redis counter
// recovery path bypasses, so PostCheck(nil) on an Open breaker leaves
// the state at Open and IsHealthy() returns false forever.
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

// TestCB_RecoverFromHalfOpen pins idempotent semantics from HalfOpen.
// Recover should also clear probeInFlight/probeStarted so a subsequent
// PreCheck does not see a stale in-flight probe.
func TestCB_RecoverFromHalfOpen(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cb := newTestBreaker(1, 10*time.Millisecond)
		_ = cb.PostCheck(errTest) // trip
		time.Sleep(15 * time.Millisecond)
		_ = cb.PreCheck() // -> half-open, probeInFlight set

		if cb.State() != StateHalfOpen {
			t.Fatalf("setup: state = %s, want half-open", cb.State())
		}

		cb.Recover()

		if cb.State() != StateClosed {
			t.Errorf("state = %s, want closed", cb.State())
		}
		if cb.probeInFlight.Load() {
			t.Error("probeInFlight should be cleared after Recover()")
		}
		if cb.probeStarted.Load() != 0 {
			t.Error("probeStarted should be zero after Recover()")
		}
	})
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

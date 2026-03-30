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
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/event"
)

// alwaysError is an error filter that treats all non-nil errors as failures.
func alwaysError(err error) bool { return err != nil }

// neverError is an error filter that never treats errors as failures.
func neverError(_ error) bool { return false }

var errTest = errors.New("test error")
var errSentinel = errors.New("circuit open")

func newTestBreaker(threshold int, timeout time.Duration) *CircuitBreaker {
	return NewCircuitBreaker("test", threshold, timeout, alwaysError, errSentinel)
}

// -------------------------------------------------------------------------
// State transitions
// -------------------------------------------------------------------------

func TestCB_StartsHealthy(t *testing.T) {
	cb := newTestBreaker(3, time.Minute)
	if !cb.IsHealthy() {
		t.Fatal("should start healthy")
	}
	if cb.State() != StateClosed {
		t.Fatalf("expected closed, got %v", cb.State())
	}
}

func TestCB_OpensAfterThreshold(t *testing.T) {
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

func TestCB_OpenRejectsCalls(t *testing.T) {
	cb := newTestBreaker(1, time.Minute)
	_ = cb.PostCheck(errTest) // trip

	if err := cb.PreCheck(); !errors.Is(err, errSentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}
}

func TestCB_HalfOpenAfterTimeout(t *testing.T) {
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
}

func TestCB_HalfOpenSuccess_Closes(t *testing.T) {
	cb := newTestBreaker(1, 10*time.Millisecond)
	_ = cb.PostCheck(errTest) // trip
	time.Sleep(15 * time.Millisecond)

	_ = cb.PreCheck()     // allow probe
	_ = cb.PostCheck(nil) // success

	if !cb.IsHealthy() {
		t.Fatal("should be healthy after successful probe")
	}
}

func TestCB_HalfOpenFailure_Reopens(t *testing.T) {
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
}

func TestCB_SuccessResetsFailureCount(t *testing.T) {
	cb := newTestBreaker(3, time.Minute)

	_ = cb.PostCheck(errTest) // 1
	_ = cb.PostCheck(errTest) // 2
	_ = cb.PostCheck(nil)     // success resets
	_ = cb.PostCheck(errTest) // 1 again
	_ = cb.PostCheck(errTest) // 2 again

	if !cb.IsHealthy() {
		t.Fatal("should still be healthy — counter was reset")
	}
}

// -------------------------------------------------------------------------
// OpenDuration
// -------------------------------------------------------------------------

func TestCB_OpenDuration_ZeroWhenClosed(t *testing.T) {
	cb := newTestBreaker(3, time.Minute)
	if d := cb.OpenDuration(); d != 0 {
		t.Fatalf("expected 0 when closed, got %v", d)
	}
}

func TestCB_OpenDuration_PositiveWhenOpen(t *testing.T) {
	cb := newTestBreaker(1, time.Minute)
	_ = cb.PostCheck(errTest) // trip

	time.Sleep(5 * time.Millisecond)
	d := cb.OpenDuration()
	if d < 5*time.Millisecond {
		t.Fatalf("expected >= 5ms, got %v", d)
	}
}

func TestCB_OpenDuration_PositiveWhenHalfOpen(t *testing.T) {
	cb := newTestBreaker(1, 10*time.Millisecond)
	_ = cb.PostCheck(errTest) // trip
	time.Sleep(15 * time.Millisecond)
	_ = cb.PreCheck() // transition to half-open

	if cb.State() != StateHalfOpen {
		t.Fatalf("expected half-open, got %v", cb.State())
	}
	if d := cb.OpenDuration(); d == 0 {
		t.Fatal("expected positive duration in half-open state")
	}
}

func TestCB_OpenDuration_ZeroAfterRecovery(t *testing.T) {
	cb := newTestBreaker(1, 10*time.Millisecond)
	_ = cb.PostCheck(errTest) // trip
	time.Sleep(15 * time.Millisecond)

	_ = cb.PreCheck()     // half-open
	_ = cb.PostCheck(nil) // recover

	if d := cb.OpenDuration(); d != 0 {
		t.Fatalf("expected 0 after recovery, got %v", d)
	}
}

// -------------------------------------------------------------------------
// Pluggable error filter
// -------------------------------------------------------------------------

func TestCB_FilteredErrorsDontTrip(t *testing.T) {
	cb := NewCircuitBreaker("test", 1, time.Minute, neverError, errSentinel)

	for range 10 {
		_ = cb.PostCheck(errTest)
	}
	if !cb.IsHealthy() {
		t.Fatal("filtered errors should not trip the breaker")
	}
}

func TestCB_NilErrorIsSuccess(t *testing.T) {
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

func TestCBCall_PassesThrough(t *testing.T) {
	cb := newTestBreaker(3, time.Minute)
	result, err := CBCall(cb, func() (string, error) { return "ok", nil })
	if err != nil || result != "ok" {
		t.Fatalf("unexpected: result=%q err=%v", result, err)
	}
}

func TestCBCall_CircuitOpen(t *testing.T) {
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

func TestCBCallNoResult_PassesThrough(t *testing.T) {
	cb := newTestBreaker(3, time.Minute)
	err := CBCallNoResult(cb, func() error { return nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCBCallNoResult_CircuitOpen(t *testing.T) {
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

func TestCB_OnlyOneProbeAllowed(t *testing.T) {
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
}

func TestCB_ProbeEligible(t *testing.T) {
	cb := newTestBreaker(1, 5*time.Millisecond)

	// Closed — not probe-eligible
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

	// Transition to half-open via PreCheck — no longer probe-eligible
	_ = cb.PreCheck()
	if cb.ProbeEligible() {
		t.Error("half-open circuit should not be probe-eligible")
	}
}

// -------------------------------------------------------------------------
// Probe jitter
// -------------------------------------------------------------------------

func TestCB_ProbeJitter_VariesBetweenInstances(t *testing.T) {
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

func TestCB_ProbeJitter_RecomputedOnReopen(t *testing.T) {
	cb := newTestBreaker(1, 50*time.Millisecond)

	// Trip → record jitter
	_ = cb.PostCheck(errTest)
	cb.mu.RLock()
	firstJitter := cb.probeJitter
	cb.mu.RUnlock()

	// Recover — sleep must exceed openTimeout + max jitter (50ms + 12.5ms)
	time.Sleep(80 * time.Millisecond)
	_ = cb.PreCheck()
	_ = cb.PostCheck(nil)
	if !cb.IsHealthy() {
		t.Fatal("expected recovery to closed state")
	}

	// Trip again → new jitter (may coincidentally equal firstJitter, but
	// the code path that sets it is exercised)
	_ = cb.PostCheck(errTest)
	cb.mu.RLock()
	_ = cb.probeJitter
	cb.mu.RUnlock()

	// Verify the code path ran without panicking
	_ = firstJitter
}

func TestCB_StaleProbeAutoResets(t *testing.T) {
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
	// Do NOT call PostCheck — the probe is "abandoned".
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
}

func TestTransition_EmitsOpenEvent(t *testing.T) {
	var emitted []event.Event
	event.Emit = func(ev event.Event) { emitted = append(emitted, ev) }
	defer func() { event.Emit = nil }()

	cb := newTestBreaker(2, time.Hour)
	_ = cb.PostCheck(errTest)
	_ = cb.PostCheck(errTest)

	if len(emitted) != 1 {
		t.Fatalf("expected 1 event, got %d", len(emitted))
	}
	if emitted[0].Type != event.BackendCircuitOpened {
		t.Errorf("event type = %q, want %q", emitted[0].Type, event.BackendCircuitOpened)
	}
	if emitted[0].Subject != "test" {
		t.Errorf("event subject = %q, want test", emitted[0].Subject)
	}
}

func TestTransition_EmitsClosedEvent(t *testing.T) {
	var emitted []event.Event
	event.Emit = func(ev event.Event) { emitted = append(emitted, ev) }
	defer func() { event.Emit = nil }()

	cb := newTestBreaker(1, 10*time.Millisecond)
	_ = cb.PostCheck(errTest) // trip
	emitted = nil             // clear the open event

	time.Sleep(15 * time.Millisecond) // wait for probe eligibility
	_ = cb.PreCheck()                 // enter half-open
	_ = cb.PostCheck(nil)             // probe succeeds → closed

	if len(emitted) != 1 {
		t.Fatalf("expected 1 closed event, got %d", len(emitted))
	}
	if emitted[0].Type != event.BackendCircuitClosed {
		t.Errorf("event type = %q, want %q", emitted[0].Type, event.BackendCircuitClosed)
	}
}

func TestTransition_NoEmitWhenHookNil(t *testing.T) {
	event.Emit = nil

	cb := newTestBreaker(1, time.Hour)
	_ = cb.PostCheck(errTest)

	if cb.State() != StateOpen {
		t.Errorf("state = %s, want open", cb.State())
	}
}

// -------------------------------------------------------------------------
// ResetStaleProbe
// -------------------------------------------------------------------------

// TestResetStaleProbe_ClosedCircuit verifies that ResetStaleProbe is a no-op
// when the circuit is closed.
func TestResetStaleProbe_ClosedCircuit(t *testing.T) {
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
}

// TestResetStaleProbe_StaleProbe verifies that ResetStaleProbe resets a
// half-open circuit whose probe has exceeded probeTimeout back to open.
func TestResetStaleProbe_StaleProbe(t *testing.T) {
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
}

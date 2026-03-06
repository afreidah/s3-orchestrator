// -------------------------------------------------------------------------------
// Circuit Breaker Core Tests
//
// Author: Alex Freidah
//
// Tests for the generic CircuitBreaker state machine: state transitions,
// pluggable error filter, concurrent probe serialization, and call helpers.
// -------------------------------------------------------------------------------

package storage

import (
	"errors"
	"sync"
	"testing"
	"time"
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
	if cb.State() != stateClosed {
		t.Fatalf("expected closed, got %v", cb.State())
	}
}

func TestCB_OpensAfterThreshold(t *testing.T) {
	cb := newTestBreaker(3, time.Minute)

	for i := 0; i < 2; i++ {
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
	if cb.State() != stateHalfOpen {
		t.Fatalf("expected half-open, got %v", cb.State())
	}
}

func TestCB_HalfOpenSuccess_Closes(t *testing.T) {
	cb := newTestBreaker(1, 10*time.Millisecond)
	_ = cb.PostCheck(errTest) // trip
	time.Sleep(15 * time.Millisecond)

	_ = cb.PreCheck()          // allow probe
	_ = cb.PostCheck(nil)      // success

	if !cb.IsHealthy() {
		t.Fatal("should be healthy after successful probe")
	}
}

func TestCB_HalfOpenFailure_Reopens(t *testing.T) {
	cb := newTestBreaker(1, 10*time.Millisecond)
	_ = cb.PostCheck(errTest) // trip
	time.Sleep(15 * time.Millisecond)

	_ = cb.PreCheck()             // allow probe
	_ = cb.PostCheck(errTest)     // probe fails

	if cb.IsHealthy() {
		t.Fatal("should be unhealthy after failed probe")
	}
	if cb.State() != stateOpen {
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

	if cb.State() != stateHalfOpen {
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

	for i := 0; i < 10; i++ {
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
	for i := 0; i < goroutines; i++ {
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

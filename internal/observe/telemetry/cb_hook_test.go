// -------------------------------------------------------------------------------
// Circuit Breaker Hook Tests
//
// Author: Alex Freidah
//
// Verifies that the telemetry → breaker bridge populates the gauge,
// counter, and event bus on the appropriate transitions. Lives here (not
// in internal/breaker) so the breaker package itself stays free of
// observability dependencies.
// -------------------------------------------------------------------------------

package telemetry

import (
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
)

// TestNewCircuitBreakerHook_EmitsOpenEvent confirms the closed→open
// transition emits a BackendCircuitOpened event with failure context.
func TestNewCircuitBreakerHook_EmitsOpenEvent(t *testing.T) {
	var emitted []event.Event
	event.Emit = func(ev event.Event) { emitted = append(emitted, ev) }
	defer func() { event.Emit = nil }()

	hook := NewCircuitBreakerHook("test-open")
	hook(breaker.StateChangeInfo{
		Name:      "test-open",
		From:      breaker.StateClosed,
		To:        breaker.StateOpen,
		Failures:  3,
		Threshold: 3,
	})

	if len(emitted) != 1 {
		t.Fatalf("expected 1 event, got %d", len(emitted))
	}
	if emitted[0].Type != event.BackendCircuitOpened {
		t.Errorf("event type = %q, want %q", emitted[0].Type, event.BackendCircuitOpened)
	}
	if emitted[0].Subject != "test-open" {
		t.Errorf("subject = %q, want test-open", emitted[0].Subject)
	}
	if got, _ := emitted[0].Data["failures"].(int); got != 3 {
		t.Errorf("failures = %v, want 3", emitted[0].Data["failures"])
	}
}

// TestNewCircuitBreakerHook_EmitsClosedEvent confirms the *→closed
// transition emits a BackendCircuitClosed event with degraded duration.
func TestNewCircuitBreakerHook_EmitsClosedEvent(t *testing.T) {
	var emitted []event.Event
	event.Emit = func(ev event.Event) { emitted = append(emitted, ev) }
	defer func() { event.Emit = nil }()

	hook := NewCircuitBreakerHook("test-closed")
	hook(breaker.StateChangeInfo{
		Name:         "test-closed",
		From:         breaker.StateHalfOpen,
		To:           breaker.StateClosed,
		OpenDuration: 250 * time.Millisecond,
	})

	if len(emitted) != 1 {
		t.Fatalf("expected 1 event, got %d", len(emitted))
	}
	if emitted[0].Type != event.BackendCircuitClosed {
		t.Errorf("event type = %q, want %q", emitted[0].Type, event.BackendCircuitClosed)
	}
}

// TestNewCircuitBreakerHook_QuietForInternalTransitions confirms that
// half-open and reopen-from-half-open transitions don't emit events;
// only operator-visible state changes go on the bus.
func TestNewCircuitBreakerHook_QuietForInternalTransitions(t *testing.T) {
	var emitted []event.Event
	event.Emit = func(ev event.Event) { emitted = append(emitted, ev) }
	defer func() { event.Emit = nil }()

	hook := NewCircuitBreakerHook("test-quiet")
	hook(breaker.StateChangeInfo{Name: "test-quiet", From: breaker.StateOpen, To: breaker.StateHalfOpen})
	hook(breaker.StateChangeInfo{Name: "test-quiet", From: breaker.StateHalfOpen, To: breaker.StateOpen})

	if len(emitted) != 0 {
		t.Errorf("expected no events for half-open / reopen transitions, got %d", len(emitted))
	}
}

// TestNewCircuitBreakerHook_NilEmitIsSafe confirms the hook does not
// panic when no event bus has been wired up.
func TestNewCircuitBreakerHook_NilEmitIsSafe(t *testing.T) {
	event.Emit = nil
	hook := NewCircuitBreakerHook("test-nil")
	hook(breaker.StateChangeInfo{
		Name: "test-nil",
		From: breaker.StateClosed,
		To:   breaker.StateOpen,
	})
}
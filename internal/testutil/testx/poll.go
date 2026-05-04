// -------------------------------------------------------------------------------
// Test Polling Helpers
//
// Author: Alex Freidah
//
// Replacements for bare time.Sleep in tests. Polling until a condition holds
// is more reliable than a fixed sleep: it finishes as soon as the state
// changes (fast on fast machines) and waits longer on slow CI before failing
// the test cleanly with a readable error message.
// -------------------------------------------------------------------------------

// Package testx holds reusable test helpers shared across packages,
// currently polling primitives that replace fixed time.Sleep calls
// with conditions evaluated until they hold and context utilities for
// scoping test deadlines.
package testx

import (
	"testing"
	"time"
)

// DefaultPollInterval is the tick cadence used by Eventually when no custom
// interval is provided. Small enough to finish tests promptly, large enough
// that a hot-looping condition doesn't burn CPU on slow machines.
const DefaultPollInterval = 5 * time.Millisecond

// Eventually repeatedly calls cond until it returns true or timeout elapses.
// Fails the test with the given message (formatted with additional args) if
// the condition never becomes true. Replaces `time.Sleep(X); assert(...)`
// patterns with a fail-fast, flake-resistant alternative.
func Eventually(t *testing.T, timeout time.Duration, cond func() bool, msgAndArgs ...any) {
	t.Helper()
	EventuallyInterval(t, timeout, DefaultPollInterval, cond, msgAndArgs...)
}

// EventuallyInterval is Eventually with a caller-supplied poll interval.
func EventuallyInterval(t *testing.T, timeout, interval time.Duration, cond func() bool, msgAndArgs ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			if len(msgAndArgs) > 0 {
				if msg, ok := msgAndArgs[0].(string); ok {
					t.Fatalf("Eventually: condition never held within %s: "+msg, append([]any{timeout}, msgAndArgs[1:]...)...)
				}
			}
			t.Fatalf("Eventually: condition never held within %s", timeout)
		}
		time.Sleep(interval)
	}
}

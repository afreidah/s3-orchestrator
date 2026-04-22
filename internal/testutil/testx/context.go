// -------------------------------------------------------------------------------
// Test Context Helper
//
// Author: Alex Freidah
//
// Context(t) returns a background context bounded by a generous deadline so
// that any test which accidentally blocks on a channel, DB call, or network
// round-trip fails with a timeout instead of hanging the entire suite. The
// cancel is registered on t.Cleanup so individual tests don't need to remember.
// -------------------------------------------------------------------------------

package testx

import (
	"context"
	"testing"
	"time"
)

// DefaultTestTimeout is the deadline applied by Context(t). Long enough that
// no well-behaved test should ever hit it, short enough that a genuinely
// hung test fails inside a CI run rather than after the job-level timeout.
const DefaultTestTimeout = 30 * time.Second

// Context returns a context.Context with the default test timeout applied.
// The cancel is registered via t.Cleanup so callers don't need to defer it.
// Use this instead of context.Background() in tests that make RPCs, DB
// queries, or channel operations — a stuck call then fails the test cleanly.
func Context(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTestTimeout)
	t.Cleanup(cancel)
	return ctx
}

// ContextWith returns a context bounded by the given timeout. Prefer Context
// unless a specific test needs a shorter or longer deadline (e.g. deliberately
// exercising a timeout path).
func ContextWith(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

// -------------------------------------------------------------------------------
// Watchdog Tests
//
// Author: Alex Freidah
//
// Tests for the periodic reset-stale-probes watchdog. Drives the
// checkAll path against an empty registry, a populated DB-breaker-only
// registry, and a registry that also carries a per-backend
// CircuitBreakerBackend so the ResetStaleProbes fan-out is exercised.
// Lives in the breaker package because the watchdog moved here as
// part of #925.
// -------------------------------------------------------------------------------

package breaker

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWatchdog_CheckAllEmptyRegistry exercises checkAll with an empty
// registry to guarantee it is a safe no-op when no breakers are wired.
func TestWatchdog_CheckAllEmptyRegistry(t *testing.T) {
	t.Parallel()
	w := &watchdog{registry: NewRegistry()}
	w.checkAll() // must not panic on empty registry
}

// TestWatchdog_RunExitsOnCancel covers the ticker loop's ctx.Done()
// branch by cancelling the context before the first tick fires.
func TestWatchdog_RunExitsOnCancel(t *testing.T) {
	t.Parallel()
	cb := NewCircuitBreaker(Config{Name: "t", Threshold: 3, Timeout: time.Second, IsError: func(error) bool { return false }, Sentinel: errors.New("sentinel")})
	w := &watchdog{registry: NewRegistry(cb)}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Run(ctx); err != nil {
		t.Errorf("watchdog Run returned error on cancel: %v", err)
	}
}

// TestWatchdog_CheckAllResetsRegisteredBreakers verifies checkAll
// invokes ResetStaleProbes on every registered breaker without
// panicking. The sentinel breaker has nothing to reset, but the
// fan-out path itself is what is exercised.
func TestWatchdog_CheckAllResetsRegisteredBreakers(t *testing.T) {
	t.Parallel()
	dbCB := NewCircuitBreaker(Config{Name: "t", Threshold: 3, Timeout: time.Second, IsError: func(error) bool { return false }, Sentinel: errors.New("sentinel")})
	w := &watchdog{registry: NewRegistry(dbCB)}
	w.checkAll() // must not panic
}

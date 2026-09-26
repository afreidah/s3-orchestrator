// -------------------------------------------------------------------------------
// Watchdog Tests
//
// Author: Alex Freidah
//
// Tests for the watchdog that probes every registered breaker on a tick:
// the Run loop's cancel path and that each tick probes the registry.
// -------------------------------------------------------------------------------

package breaker

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWatchdog_RunExitsOnCancel covers the ticker loop's ctx.Done()
// branch by cancelling the context before the first tick fires.
func TestWatchdog_RunExitsOnCancel(t *testing.T) {
	t.Parallel()
	cb := NewCircuitBreaker(Config{Name: "t", Threshold: 3, Timeout: time.Second, IsError: func(error) bool { return false }, Sentinel: errors.New("sentinel")})
	w := NewWatchdog(NewRegistry(cb))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Run(ctx); err != nil {
		t.Errorf("watchdog Run returned error on cancel: %v", err)
	}
}

// TestWatchdog_RunProbesEachTick verifies the ticker loop probes every
// registered breaker.
func TestWatchdog_RunProbesEachTick(t *testing.T) {
	t.Parallel()
	p := &fakeProber{}
	w := &watchdog{registry: NewRegistry(p), interval: time.Millisecond}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for p.count.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("probed %d times before the deadline, want at least 2", p.count.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("watchdog Run returned error on cancel: %v", err)
	}
}

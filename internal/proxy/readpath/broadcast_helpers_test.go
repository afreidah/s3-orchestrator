// -------------------------------------------------------------------------------
// Broadcast Helper Tests - Direct Coverage of Extracted Pure Helpers
//
// Author: Alex Freidah
//
// The branch entry points (tryBackendsSequentially, tryBackendsInParallel)
// are covered end-to-end by TestGetObject_DBUnavailable_* and
// TestGetObject_ParallelBroadcast_* in manager_objects_test.go. These tests
// pin the small extracted helpers directly.
// -------------------------------------------------------------------------------

package readpath

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// noopSpan returns a no-op trace.Span suitable for helpers that only call
// SetStatus / RecordError / SetAttributes for observability side effects.
func noopSpan() trace.Span {
	return trace.SpanFromContext(context.Background())
}

// -------------------------------------------------------------------------
// broadcastAllFailed
// -------------------------------------------------------------------------

// TestBroadcastAllFailed_NilErrorReturnsObjectNotFound verifies the broadcast all failed nil error returns object not found contract.
// Asserts that name = , want empty.
func TestBroadcastAllFailed_NilErrorReturnsObjectNotFound(t *testing.T) {
	t.Parallel()
	name, err := broadcastAllFailed(noopSpan(), nil)
	if name != "" {
		t.Errorf("name = %q, want empty", name)
	}
	if !errors.Is(err, core.ErrObjectNotFound) {
		t.Errorf("err = %v, want chain containing ErrObjectNotFound", err)
	}
}

// TestBroadcastAllFailed_WrapsLastError verifies the broadcast all failed wraps last error contract.
// Asserts that name = , want empty.
func TestBroadcastAllFailed_WrapsLastError(t *testing.T) {
	t.Parallel()
	lastErr := errors.New("backend on fire")
	name, err := broadcastAllFailed(noopSpan(), lastErr)
	if name != "" {
		t.Errorf("name = %q, want empty", name)
	}
	if !errors.Is(err, lastErr) {
		t.Errorf("err chain missing underlying: %v", err)
	}
	if !strings.Contains(err.Error(), "all backends failed during degraded read") {
		t.Errorf("err missing wrapping prefix: %v", err)
	}
}

// -------------------------------------------------------------------------
// drainAndCleanupLosers
// -------------------------------------------------------------------------

// TestDrainAndCleanupLosers_InvokesCleanupOnEachResult verifies that every
// loser result drained from the channel has its cleanup func invoked, so
// per-call timeout contexts are released promptly instead of leaking until
// the deadline fires.
func TestDrainAndCleanupLosers_InvokesCleanupOnEachResult(t *testing.T) {
	t.Parallel()
	ch := make(chan broadcastResult, 3)
	var cleanups atomic.Int32
	mkCleanup := func() func() { return func() { cleanups.Add(1) } }

	ch <- broadcastResult{name: "a", cleanup: mkCleanup()}
	ch <- broadcastResult{name: "b", cleanup: mkCleanup()}
	ch <- broadcastResult{name: "c", cleanup: mkCleanup()}

	drainAndCleanupLosers(context.Background(), "GET", ch, 3, time.Second)
	if got := cleanups.Load(); got != 3 {
		t.Errorf("cleanup invocations = %d, want 3", got)
	}
}

// TestDrainAndCleanupLosers_ToleratesNilCleanup verifies the drain skips
// results whose cleanup is nil (errored probes) without panicking.
func TestDrainAndCleanupLosers_ToleratesNilCleanup(t *testing.T) {
	t.Parallel()
	ch := make(chan broadcastResult, 2)
	var cleanups atomic.Int32
	cleanupFn := func() { cleanups.Add(1) }

	ch <- broadcastResult{name: "errored", err: errors.New("fail")} // nil cleanup
	ch <- broadcastResult{name: "won", cleanup: cleanupFn}

	drainAndCleanupLosers(context.Background(), "GET", ch, 2, time.Second)
	if got := cleanups.Load(); got != 1 {
		t.Errorf("cleanup invocations = %d, want 1 (nil cleanup skipped)", got)
	}
}

// TestDrainAndCleanupLosers_TimesOutOnHungProbe is the regression test for the
// goroutine leak: when a cancelled probe never sends its result, the drain must
// return at its timeout instead of blocking forever, count the timeout, and
// still run cleanup for the loser that did report. The unique operation label
// isolates the counter assertion from other parallel tests.
func TestDrainAndCleanupLosers_TimesOutOnHungProbe(t *testing.T) {
	t.Parallel()
	const op = "drain-timeout-test"
	ch := make(chan broadcastResult, 2)
	var cleanups atomic.Int32
	ch <- broadcastResult{name: "reported", cleanup: func() { cleanups.Add(1) }}
	// The second probe is "hung": it never sends, so remaining (2) exceeds
	// what arrives. The old unbounded drain blocked here forever.

	before := testutil.ToFloat64(telemetry.DegradedBroadcastDrainTimeoutTotal.WithLabelValues(op))

	done := make(chan struct{})
	go func() {
		drainAndCleanupLosers(context.Background(), op, ch, 2, 20*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainAndCleanupLosers did not return; the timeout bound is broken")
	}

	if got := cleanups.Load(); got != 1 {
		t.Errorf("cleanup invocations = %d, want 1 (the reported loser)", got)
	}
	if after := testutil.ToFloat64(telemetry.DegradedBroadcastDrainTimeoutTotal.WithLabelValues(op)); after != before+1 {
		t.Errorf("drain-timeout counter delta = %v, want 1", after-before)
	}
}

// TestDrainAndCleanupLosers_ReturnsOnContextCancel verifies the drain also
// exits promptly when the request context is cancelled, without waiting out
// the (here, very long) timeout.
func TestDrainAndCleanupLosers_ReturnsOnContextCancel(t *testing.T) {
	t.Parallel()
	ch := make(chan broadcastResult) // unbuffered; nothing is ever sent
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		drainAndCleanupLosers(ctx, "GET", ch, 1, time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainAndCleanupLosers ignored ctx cancellation")
	}
}

// -------------------------------------------------------------------------
// cancelLosers
// -------------------------------------------------------------------------

// TestCancelLosers_LeavesWinnerAliveCancelsRest verifies that the winner's
// context is preserved (its response body is still being read) while every
// other per-probe context is cancelled so losing backends stop wasting CPU
// and API calls.
func TestCancelLosers_LeavesWinnerAliveCancelsRest(t *testing.T) {
	t.Parallel()
	parent := context.Background()
	ctxA, cancelA := context.WithCancel(parent)
	defer cancelA()
	ctxB, cancelB := context.WithCancel(parent)
	defer cancelB()
	ctxC, cancelC := context.WithCancel(parent)
	defer cancelC()

	cancels := map[string]context.CancelFunc{
		"a": cancelA,
		"b": cancelB,
		"c": cancelC,
	}

	cancelLosers(cancels, "b")

	if ctxA.Err() == nil {
		t.Errorf("loser a was not cancelled")
	}
	if ctxB.Err() != nil {
		t.Errorf("winner b was cancelled: %v", ctxB.Err())
	}
	if ctxC.Err() == nil {
		t.Errorf("loser c was not cancelled")
	}
	// Release the winner so the linter does not flag a leak in the test.
	cancelB()
}

// TestCancelLosers_UnknownWinnerCancelsEveryone verifies the defensive
// behavior when the winner's name does not match any tracked probe (which
// should not happen in practice). Every cancel still fires so no per-probe
// context leaks.
func TestCancelLosers_UnknownWinnerCancelsEveryone(t *testing.T) {
	t.Parallel()
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	cancelLosers(map[string]context.CancelFunc{"a": cancelA, "b": cancelB}, "no-such-backend")

	if ctxA.Err() == nil || ctxB.Err() == nil {
		t.Errorf("expected both contexts cancelled, got a=%v b=%v", ctxA.Err(), ctxB.Err())
	}
}

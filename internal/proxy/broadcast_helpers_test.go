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

package proxy

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"go.opentelemetry.io/otel/trace"
)

// noopSpan returns a no-op trace.Span suitable for helpers that only call
// SetStatus / RecordError / SetAttributes for observability side effects.
func noopSpan() trace.Span {
	return trace.SpanFromContext(context.Background())
}

// -------------------------------------------------------------------------
// broadcastAllFailed
// -------------------------------------------------------------------------

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

	drainAndCleanupLosers(ch, 3)
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

	drainAndCleanupLosers(ch, 2)
	if got := cleanups.Load(); got != 1 {
		t.Errorf("cleanup invocations = %d, want 1 (nil cleanup skipped)", got)
	}
}

// -------------------------------------------------------------------------
// bodyWithCancel
// -------------------------------------------------------------------------

// stringReadCloser is a minimal io.ReadCloser that records the number of
// times Close has been invoked. Used to verify bodyWithCancel forwards the
// underlying Close as well as firing the cancel.
type stringReadCloser struct {
	*strings.Reader
	closes atomic.Int32
}

// Close records the call and returns nil.
func (s *stringReadCloser) Close() error {
	s.closes.Add(1)
	return nil
}

// TestBodyWithCancel_ClosesAndCancels verifies the wrapper forwards Close
// to the underlying body AND invokes the cancel func once. Without this
// the read-path callbacks would leak per-call timeout contexts on every
// successful streaming GetObject.
func TestBodyWithCancel_ClosesAndCancels(t *testing.T) {
	t.Parallel()
	inner := &stringReadCloser{Reader: strings.NewReader("payload")}
	var cancels atomic.Int32
	wrapped := bodyWithCancel(inner, func() { cancels.Add(1) })

	if err := wrapped.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := inner.closes.Load(); got != 1 {
		t.Errorf("inner Close calls = %d, want 1", got)
	}
	if got := cancels.Load(); got != 1 {
		t.Errorf("cancel invocations = %d, want 1", got)
	}
}

// TestBodyWithCancel_IdempotentClose verifies double-Close fires cancel
// exactly once. Lets callers defer Close without worrying about whether
// the body has already been closed earlier in the response path.
func TestBodyWithCancel_IdempotentClose(t *testing.T) {
	t.Parallel()
	inner := &stringReadCloser{Reader: strings.NewReader("x")}
	var cancels atomic.Int32
	wrapped := bodyWithCancel(inner, func() { cancels.Add(1) })

	_ = wrapped.Close()
	_ = wrapped.Close()
	if got := cancels.Load(); got != 1 {
		t.Errorf("cancel invocations after double-close = %d, want 1", got)
	}
}

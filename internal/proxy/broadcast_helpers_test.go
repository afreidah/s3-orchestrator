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

	"go.opentelemetry.io/otel/trace"

	st "github.com/afreidah/s3-orchestrator/internal/store"
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
	if !errors.Is(err, st.ErrObjectNotFound) {
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
// drainAndCancelLosers
// -------------------------------------------------------------------------

func TestDrainAndCancelLosers_InvokesCancelOnEachResult(t *testing.T) {
	t.Parallel()
	ch := make(chan broadcastResult, 3)
	var cancels atomic.Int32
	mkCancel := func() context.CancelFunc { return func() { cancels.Add(1) } }

	ch <- broadcastResult{name: "a", cancel: mkCancel()}
	ch <- broadcastResult{name: "b", cancel: mkCancel()}
	ch <- broadcastResult{name: "c", cancel: mkCancel()}

	drainAndCancelLosers(ch, 3)
	if got := cancels.Load(); got != 3 {
		t.Errorf("cancel invocations = %d, want 3", got)
	}
}

func TestDrainAndCancelLosers_TolerestNilCancel(t *testing.T) {
	t.Parallel()
	ch := make(chan broadcastResult, 2)
	var cancels atomic.Int32
	cancelFn := func() { cancels.Add(1) }

	ch <- broadcastResult{name: "errored", err: errors.New("fail")} // nil cancel
	ch <- broadcastResult{name: "won", cancel: cancelFn}

	drainAndCancelLosers(ch, 2)
	if got := cancels.Load(); got != 1 {
		t.Errorf("cancel invocations = %d, want 1 (nil cancel skipped)", got)
	}
}

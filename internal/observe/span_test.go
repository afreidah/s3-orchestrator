// -------------------------------------------------------------------------------
// observe.Run unit tests
//
// Author: Alex Freidah
//
// Pin the invariants every caller relies on: spans always close, recorders
// fire exactly once with the correct outcome, status codes match
// success/failure, and Run propagates fn's return values verbatim.
// -------------------------------------------------------------------------------

package observe

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestRun_HappyPath confirms the closure result and a nil error flow back to
// the caller, and the recorder sees a nil error with a non-zero start time.
func TestRun_HappyPath(t *testing.T) {
	t.Parallel()
	var seenErr error
	var seenOp string
	var seenStart time.Time
	rec := func(op string, start time.Time, err error) {
		seenOp, seenStart, seenErr = op, start, err
	}

	got, err := Run(context.Background(), Internal("op", nil, rec), func(_ context.Context) (int, error) {
		return 42, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42 {
		t.Errorf("got %d, want 42", got)
	}
	if seenOp != "op" {
		t.Errorf("recorder op = %q, want op", seenOp)
	}
	if seenErr != nil {
		t.Errorf("recorder err = %v, want nil", seenErr)
	}
	if seenStart.IsZero() {
		t.Error("recorder start was zero")
	}
}

// TestRun_PropagatesError ensures a closure error flows through unchanged
// and the recorder observes it.
func TestRun_PropagatesError(t *testing.T) {
	t.Parallel()
	want := errors.New("boom")
	var seen error
	rec := func(_ string, _ time.Time, err error) { seen = err }

	_, err := Run(context.Background(), Client("op", nil, rec), func(_ context.Context) (string, error) {
		return "", want
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
	if !errors.Is(seen, want) {
		t.Fatalf("recorder saw %v, want %v", seen, want)
	}
}

// TestRun_NilRecorderAllowed verifies a nil Recorder is treated as a no-op.
// Workers that emit only spans (no metrics) rely on this.
func TestRun_NilRecorderAllowed(t *testing.T) {
	t.Parallel()
	got, err := Run(context.Background(), Internal("op", nil, nil), func(_ context.Context) (int, error) {
		return 7, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 7 {
		t.Errorf("got %d, want 7", got)
	}
}

// TestRun_RecorderFiresOnPanic enforces the lifecycle guarantee that a
// panicking closure still triggers metric/span finalization before the
// panic propagates.
func TestRun_RecorderFiresOnPanic(t *testing.T) {
	t.Parallel()
	var fired atomic.Bool
	rec := func(_ string, _ time.Time, _ error) { fired.Store(true) }

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic to propagate")
		}
		if !fired.Load() {
			t.Error("recorder did not fire before panic propagated")
		}
	}()

	_, _ = Run(context.Background(), Server("op", nil, rec), func(_ context.Context) (int, error) {
		panic("boom")
	})
}

// TestRunErr_HappyPath exercises the void variant.
func TestRunErr_HappyPath(t *testing.T) {
	t.Parallel()
	called := false
	if err := RunErr(context.Background(), Internal("op", nil, nil), func(_ context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("closure was not invoked")
	}
}

// TestRunErr_PropagatesError pins the void-variant error path.
func TestRunErr_PropagatesError(t *testing.T) {
	t.Parallel()
	want := errors.New("nope")
	if err := RunErr(context.Background(), Internal("op", nil, nil), func(_ context.Context) error {
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

// TestConstructors_AssignSpanKind locks in the kind that each helper
// associates with the resulting Op so accidental swaps fail at unit-test
// time rather than at trace-render time.
func TestConstructors_AssignSpanKind(t *testing.T) {
	t.Parallel()
	if Client("c", nil, nil).Kind.String() != "client" {
		t.Error("Client kind != client")
	}
	if Server("s", nil, nil).Kind.String() != "server" {
		t.Error("Server kind != server")
	}
	if Internal("i", nil, nil).Kind.String() != "internal" {
		t.Error("Internal kind != internal")
	}
}
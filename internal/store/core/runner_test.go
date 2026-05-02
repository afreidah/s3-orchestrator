// -------------------------------------------------------------------------------
// Core Runner - Generic WithTxVal Tests
//
// Author: Alex Freidah
//
// Direct coverage for WithTxVal[T]: value propagation on success and zero-
// value on error. The fakeRunner exercises every branch without standing
// up an engine.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"testing"
)

// -------------------------------------------------------------------------
// FAKE RUNNER
// -------------------------------------------------------------------------

// fakeRunner runs the supplied closure synchronously, returning whatever
// error the closure yields. The supplied tx pointer is always nil since
// the WithTxVal tests do not exercise adapter behavior.
type fakeRunner struct {
	beginErr error
}

// WithTx satisfies core.Runner. Returns beginErr when set; otherwise
// invokes fn with a nil TxAdapter.
func (r *fakeRunner) WithTx(ctx context.Context, fn func(ctx context.Context, tx TxAdapter) error) error {
	if r.beginErr != nil {
		return r.beginErr
	}
	return fn(ctx, nil)
}

// -------------------------------------------------------------------------
// WithTxVal
// -------------------------------------------------------------------------

// TestWithTxVal_ReturnsValueOnSuccess verifies the helper propagates
// the closure's return value when fn returns nil error.
func TestWithTxVal_ReturnsValueOnSuccess(t *testing.T) {
	t.Parallel()
	got, err := WithTxVal(context.Background(), &fakeRunner{}, func(_ context.Context, _ TxAdapter) (int, error) {
		return 42, nil
	})
	if err != nil {
		t.Fatalf("WithTxVal: %v", err)
	}
	if got != 42 {
		t.Errorf("WithTxVal = %d, want 42", got)
	}
}

// TestWithTxVal_ZeroValueOnFnError verifies the helper returns the zero
// value of T when the closure errors, propagating the error verbatim.
func TestWithTxVal_ZeroValueOnFnError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("fn failed")
	got, err := WithTxVal(context.Background(), &fakeRunner{}, func(_ context.Context, _ TxAdapter) (int, error) {
		return 99, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
	if got != 0 {
		t.Errorf("WithTxVal = %d, want 0 (zero value on error)", got)
	}
}

// TestWithTxVal_ZeroValueOnRunnerError verifies the helper returns the
// zero value when the runner itself fails to begin a transaction.
func TestWithTxVal_ZeroValueOnRunnerError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("begin failed")
	r := &fakeRunner{beginErr: sentinel}
	got, err := WithTxVal(context.Background(), r, func(_ context.Context, _ TxAdapter) (string, error) {
		return "should-not-see", nil
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
	if got != "" {
		t.Errorf("WithTxVal = %q, want zero value", got)
	}
}

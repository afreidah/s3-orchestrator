// -------------------------------------------------------------------------------
// Backend - Stream Copy Error Tests
//
// Author: Alex Freidah
//
// Pin the contract every classifier relies on: Error renders with the phase
// prefix, Unwrap exposes the inner error to errors.Is / errors.As, and the
// IsCopyPhase helper returns true for matching phases and false for nil,
// wrapped, or unrelated errors.
// -------------------------------------------------------------------------------

package backend_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
)

var errSentinel = errors.New("sentinel")

// TestCopyError_Error renders "<phase>: <underlying>".
func TestCopyError_Error(t *testing.T) {
	t.Parallel()
	got := (&backend.CopyError{Phase: backend.CopyPhaseWrite, Err: errors.New("boom")}).Error()
	if got != "write: boom" {
		t.Errorf("Error() = %q, want %q", got, "write: boom")
	}
}

// TestCopyError_Unwrap exposes the wrapped error so errors.Is /
// errors.As walks discover sentinels in the chain.
func TestCopyError_Unwrap(t *testing.T) {
	t.Parallel()
	wrapped := &backend.CopyError{Phase: backend.CopyPhaseRead, Err: errSentinel}
	if !errors.Is(wrapped, errSentinel) {
		t.Error("errors.Is should find the wrapped sentinel")
	}
}

// TestIsCopyPhase covers the four classifier outcomes the replicator
// retry loop depends on.
func TestIsCopyPhase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		err   error
		phase backend.CopyPhase
		want  bool
	}{
		{
			name:  "matching phase",
			err:   &backend.CopyError{Phase: backend.CopyPhaseWrite, Err: errors.New("boom")},
			phase: backend.CopyPhaseWrite,
			want:  true,
		},
		{
			name:  "mismatched phase",
			err:   &backend.CopyError{Phase: backend.CopyPhaseRead, Err: errors.New("boom")},
			phase: backend.CopyPhaseWrite,
			want:  false,
		},
		{
			name:  "wrapped CopyError still matches",
			err:   fmt.Errorf("outer context: %w", &backend.CopyError{Phase: backend.CopyPhaseWrite, Err: errors.New("inner")}),
			phase: backend.CopyPhaseWrite,
			want:  true,
		},
		{
			name:  "unrelated error",
			err:   errors.New("plain error"),
			phase: backend.CopyPhaseWrite,
			want:  false,
		},
		{
			name:  "nil error",
			err:   nil,
			phase: backend.CopyPhaseWrite,
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := backend.IsCopyPhase(tc.err, tc.phase); got != tc.want {
				t.Errorf("IsCopyPhase(%v, %q) = %v, want %v", tc.err, tc.phase, got, tc.want)
			}
		})
	}
}

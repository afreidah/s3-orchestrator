// -------------------------------------------------------------------------------
// toAdminWorkerHealth Tests
//
// Author: Alex Freidah
//
// Covers the lifecycle.WorkerHealth -> admin.WorkerHealth conversion.
// The two types intentionally live in different packages so the wire
// contract owns its own shape; this is the one place where field
// drift between them would surface, so the test pins every field.
// -------------------------------------------------------------------------------

package di

import (
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
)

// TestToAdminWorkerHealth_CopiesEveryField asserts that each
// lifecycle.WorkerHealth field lands on the matching admin.WorkerHealth
// field in order. A field added to one side without the other is a
// silent wire-contract drift, so an exhaustive equality check is the
// right shape here.
func TestToAdminWorkerHealth_CopiesEveryField(t *testing.T) {
	t.Parallel()
	now := time.Unix(1715000000, 0).UTC()
	earlier := now.Add(-time.Hour)
	in := []lifecycle.WorkerHealth{
		{Name: "cleanup_queue", LastSuccess: now, ConsecutiveFailures: 0},
		{Name: "replicator", LastSuccess: earlier, LastFailure: now, LastError: "boom", ConsecutiveFailures: 4},
	}
	out := toAdminWorkerHealth(in)
	if len(out) != len(in) {
		t.Fatalf("len(out) = %d, want %d", len(out), len(in))
	}
	for i, want := range in {
		got := out[i]
		if got.Name != want.Name {
			t.Errorf("[%d] Name = %q, want %q", i, got.Name, want.Name)
		}
		if !got.LastSuccess.Equal(want.LastSuccess) {
			t.Errorf("[%d] LastSuccess = %v, want %v", i, got.LastSuccess, want.LastSuccess)
		}
		if !got.LastFailure.Equal(want.LastFailure) {
			t.Errorf("[%d] LastFailure = %v, want %v", i, got.LastFailure, want.LastFailure)
		}
		if got.LastError != want.LastError {
			t.Errorf("[%d] LastError = %q, want %q", i, got.LastError, want.LastError)
		}
		if got.ConsecutiveFailures != want.ConsecutiveFailures {
			t.Errorf("[%d] ConsecutiveFailures = %d, want %d", i, got.ConsecutiveFailures, want.ConsecutiveFailures)
		}
	}
}

// TestToAdminWorkerHealth_EmptyInputReturnsEmptySlice pins that the
// helper returns a non-nil empty slice when there are no registered
// services, so JSON encoding never serializes `null` for the
// /admin/api/workers payload.
func TestToAdminWorkerHealth_EmptyInputReturnsEmptySlice(t *testing.T) {
	t.Parallel()
	out := toAdminWorkerHealth(nil)
	if out == nil {
		t.Error("expected non-nil empty slice so JSON encodes []  not null")
	}
	if len(out) != 0 {
		t.Errorf("len(out) = %d, want 0", len(out))
	}
}

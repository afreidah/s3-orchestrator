// -------------------------------------------------------------------------------
// Async Operation Tracker Tests
//
// Author: Alex Freidah
//
// Unit tests for asyncOpTracker covering start, complete, status, duplicate
// prevention, and independent operation tracking.
// -------------------------------------------------------------------------------

package ui

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// -------------------------------------------------------------------------
// TRY START AND STATUS
// -------------------------------------------------------------------------

// TestAsyncOpTracker_TryStart verifies the async op tracker try start path by exercising tr.TryStart, tr.Status.
func TestAsyncOpTracker_TryStart(t *testing.T) {
	t.Parallel()
	var tr asyncOpTracker

	if !tr.TryStart("rebalance") {
		t.Fatal("expected TryStart to succeed")
	}

	_, running := tr.Status("rebalance")
	if !running {
		t.Error("expected running=true after TryStart")
	}
}

// TestAsyncOpTracker_TryStartDuplicate verifies the async op tracker try start duplicate path by exercising tr.TryStart.
func TestAsyncOpTracker_TryStartDuplicate(t *testing.T) {
	t.Parallel()
	var tr asyncOpTracker

	tr.TryStart("rebalance")
	if tr.TryStart("rebalance") {
		t.Fatal("expected second TryStart to fail while running")
	}
}

// TestAsyncOpTracker_IndependentOps verifies the async op tracker independent ops path by exercising tr.TryStart.
func TestAsyncOpTracker_IndependentOps(t *testing.T) {
	t.Parallel()
	var tr asyncOpTracker

	tr.TryStart("rebalance")
	if !tr.TryStart("clean-excess") {
		t.Fatal("different operations should be independent")
	}
}

// -------------------------------------------------------------------------
// COMPLETE AND RESULT
// -------------------------------------------------------------------------

// TestAsyncOpTracker_Complete verifies a completed operation is no longer
// running and keeps the report it finished with.
func TestAsyncOpTracker_Complete(t *testing.T) {
	t.Parallel()
	var tr asyncOpTracker

	tr.TryStart("rebalance")
	tr.Complete("rebalance", &admin.Report{Result: adminstream.OutcomeOK, Body: 42})

	result, running := tr.Status("rebalance")
	if running {
		t.Error("expected running=false after Complete")
	}
	if result == nil || result.Result != adminstream.OutcomeOK || result.Body != 42 {
		t.Errorf("result = %+v, want the completed report", result)
	}
}

// TestAsyncOpTracker_RestartAfterComplete verifies a finished operation may
// start again.
func TestAsyncOpTracker_RestartAfterComplete(t *testing.T) {
	t.Parallel()
	var tr asyncOpTracker

	tr.TryStart("rebalance")
	tr.Complete("rebalance", &admin.Report{Result: adminstream.OutcomeOK})

	if !tr.TryStart("rebalance") {
		t.Fatal("expected TryStart to succeed after completion")
	}
}

// -------------------------------------------------------------------------
// IDLE STATUS
// -------------------------------------------------------------------------

// TestAsyncOpTracker_StatusIdle verifies the async op tracker status idle path by exercising tr.Status.
func TestAsyncOpTracker_StatusIdle(t *testing.T) {
	t.Parallel()
	var tr asyncOpTracker

	result, running := tr.Status("rebalance")
	if running {
		t.Error("expected running=false for unknown op")
	}
	if result != nil {
		t.Error("expected nil result for unknown op")
	}
}

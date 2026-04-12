// -------------------------------------------------------------------------------
// Async Operation Tracker Tests
//
// Author: Alex Freidah
//
// Unit tests for asyncOpTracker covering start, complete, status, duplicate
// prevention, and independent operation tracking.
// -------------------------------------------------------------------------------

package ui

import "testing"

// -------------------------------------------------------------------------
// TRY START AND STATUS
// -------------------------------------------------------------------------

func TestAsyncOpTracker_TryStart(t *testing.T) {
	var tr asyncOpTracker

	if !tr.TryStart("rebalance") {
		t.Fatal("expected TryStart to succeed")
	}

	_, running := tr.Status("rebalance")
	if !running {
		t.Error("expected running=true after TryStart")
	}
}

func TestAsyncOpTracker_TryStartDuplicate(t *testing.T) {
	var tr asyncOpTracker

	tr.TryStart("rebalance")
	if tr.TryStart("rebalance") {
		t.Fatal("expected second TryStart to fail while running")
	}
}

func TestAsyncOpTracker_IndependentOps(t *testing.T) {
	var tr asyncOpTracker

	tr.TryStart("rebalance")
	if !tr.TryStart("clean-excess") {
		t.Fatal("different operations should be independent")
	}
}

// -------------------------------------------------------------------------
// COMPLETE AND RESULT
// -------------------------------------------------------------------------

func TestAsyncOpTracker_Complete(t *testing.T) {
	var tr asyncOpTracker

	tr.TryStart("rebalance")
	tr.Complete("rebalance", &asyncResult{OK: true, Count: 42})

	result, running := tr.Status("rebalance")
	if running {
		t.Error("expected running=false after Complete")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.OK || result.Count != 42 {
		t.Errorf("result = %+v, want OK=true Count=42", result)
	}
}

func TestAsyncOpTracker_CompleteError(t *testing.T) {
	var tr asyncOpTracker

	tr.TryStart("rebalance")
	tr.Complete("rebalance", &asyncResult{Error: "db down"})

	result, _ := tr.Status("rebalance")
	if result == nil || result.Error != "db down" {
		t.Errorf("expected error result, got %+v", result)
	}
}

func TestAsyncOpTracker_RestartAfterComplete(t *testing.T) {
	var tr asyncOpTracker

	tr.TryStart("rebalance")
	tr.Complete("rebalance", &asyncResult{OK: true, Count: 5})

	if !tr.TryStart("rebalance") {
		t.Fatal("expected TryStart to succeed after completion")
	}
}

// -------------------------------------------------------------------------
// IDLE STATUS
// -------------------------------------------------------------------------

func TestAsyncOpTracker_StatusIdle(t *testing.T) {
	var tr asyncOpTracker

	result, running := tr.Status("rebalance")
	if running {
		t.Error("expected running=false for unknown op")
	}
	if result != nil {
		t.Error("expected nil result for unknown op")
	}
}

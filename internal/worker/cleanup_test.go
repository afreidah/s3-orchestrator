// -------------------------------------------------------------------------------
// Cleanup Worker Tests
//
// Author: Alex Freidah
//
// Covers the cleanup-worker decision tree against a mock store and mock
// backends: successful retry, transient failure with retry scheduled,
// admission rejection, missing-backend completion, the exhaustion path
// that graduates rows to cleanup_dlq, and the DLQ-move-failure tolerance
// path. Also pins the exponential-backoff curve produced by
// CleanupBackoff across attempt counts.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"go.uber.org/mock/gomock"
)

// TestProcessCleanupQueue_DeleteSuccess verifies the process cleanup queue delete success contract.
// Asserts that expected processed=1, got.
func TestProcessCleanupQueue_DeleteSuccess(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockCleanupOps(ctrl)

	st := core.CleanupItem{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt", SizeBytes: 100}
	ms := &mockMetadataStore{pendingCleanups: []core.CleanupItem{st}}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(nil, nil) // backend value doesn't matter for this test
	ops.EXPECT().DeleteWithTimeout(gomock.Any(), gomock.Any(), "orphan.txt").Return(nil)
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()

	w := NewCleanupWorker(ops, ms, 1)
	processed, failed := w.ProcessCleanupQueue(context.Background())

	if processed != 1 {
		t.Errorf("expected processed=1, got %d", processed)
	}
	if failed != 0 {
		t.Errorf("expected failed=0, got %d", failed)
	}
}

// TestProcessCleanupQueue_DeleteFails_Retries verifies the process cleanup queue delete fails retries contract.
// Asserts that expected failed=1, got.
func TestProcessCleanupQueue_DeleteFails_Retries(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockCleanupOps(ctrl)

	st := core.CleanupItem{ID: 2, BackendName: "b1", ObjectKey: "stuck.txt", Attempts: 3}
	ms := &mockMetadataStore{pendingCleanups: []core.CleanupItem{st}}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(nil, nil)
	ops.EXPECT().DeleteWithTimeout(gomock.Any(), gomock.Any(), "stuck.txt").Return(errors.New("timeout"))
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()

	w := NewCleanupWorker(ops, ms, 1)
	_, failed := w.ProcessCleanupQueue(context.Background())

	if failed != 1 {
		t.Errorf("expected failed=1, got %d", failed)
	}
}

// TestProcessCleanupQueue_AdmissionBlocked verifies the process cleanup queue admission blocked contract.
// Asserts that expected 0/0 when blocked, got /.
func TestProcessCleanupQueue_AdmissionBlocked(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockCleanupOps(ctrl)

	st := core.CleanupItem{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt"}
	ms := &mockMetadataStore{pendingCleanups: []core.CleanupItem{st}}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(false)

	w := NewCleanupWorker(ops, ms, 1)
	processed, failed := w.ProcessCleanupQueue(context.Background())

	if processed != 0 || failed != 0 {
		t.Errorf("expected 0/0 when blocked, got %d/%d", processed, failed)
	}
}

// TestProcessCleanupQueue_BackendNotFound verifies the process cleanup queue backend not found contract.
// Asserts that expected processed=1 (item removed), got.
func TestProcessCleanupQueue_BackendNotFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockCleanupOps(ctrl)

	st := core.CleanupItem{ID: 1, BackendName: "gone", ObjectKey: "orphan.txt"}
	ms := &mockMetadataStore{pendingCleanups: []core.CleanupItem{st}}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("gone").Return(nil, errors.New("not found"))
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()

	w := NewCleanupWorker(ops, ms, 1)
	processed, _ := w.ProcessCleanupQueue(context.Background())

	if processed != 1 {
		t.Errorf("expected processed=1 (item removed), got %d", processed)
	}
}

// TestCleanupBackoff verifies the cleanup backoff contract.
// Asserts that CleanupBackoff() = , want.
func TestCleanupBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		attempts int32
		want     time.Duration
	}{
		{0, 1 * time.Minute},
		{1, 2 * time.Minute},
		{5, 32 * time.Minute},
		{11, 24 * time.Hour},
	}
	for _, tt := range tests {
		got := CleanupBackoff(tt.attempts)
		if got != tt.want {
			t.Errorf("CleanupBackoff(%d) = %v, want %v", tt.attempts, got, tt.want)
		}
	}
}

// TestProcessCleanupQueue_Exhausted_MovesToDLQ asserts that an item
// that has used its full retry budget (Attempts already at
// maxCleanupAttempts-1, so newAttempts crosses the ceiling) graduates
// into cleanup_dlq instead of staying pinned in cleanup_queue. This is
// the visibility fix for #651: post-exhaustion the row must surface
// somewhere an operator can find it.
func TestProcessCleanupQueue_Exhausted_MovesToDLQ(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockCleanupOps(ctrl)

	// Attempts=9 + 1 increment crosses maxCleanupAttempts (10), so the
	// worker takes the exhausted branch.
	st := core.CleanupItem{ID: 99, BackendName: "b1", ObjectKey: "doomed.txt", Attempts: 9, SizeBytes: 512}
	ms := &mockMetadataStore{pendingCleanups: []core.CleanupItem{st}}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(nil, nil)
	ops.EXPECT().DeleteWithTimeout(gomock.Any(), gomock.Any(), "doomed.txt").Return(errors.New("permanent failure"))
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()

	w := NewCleanupWorker(ops, ms, 1)
	_, failed := w.ProcessCleanupQueue(context.Background())

	if failed != 1 {
		t.Errorf("expected failed=1 (exhausted), got %d", failed)
	}
	if len(ms.dlqMoves) != 1 {
		t.Fatalf("expected one MoveCleanupToDLQ call, got %d", len(ms.dlqMoves))
	}
	if ms.dlqMoves[0].id != 99 {
		t.Errorf("dlq move id=%d, want 99", ms.dlqMoves[0].id)
	}
	if ms.dlqMoves[0].lastError != "permanent failure" {
		t.Errorf("dlq move lastError=%q, want %q", ms.dlqMoves[0].lastError, "permanent failure")
	}
}

// TestProcessCleanupQueue_Exhausted_DLQMoveFails asserts that the
// worker still counts the item as failed, increments the exhausted
// metric, and continues when the move-to-DLQ transaction itself fails.
// The DB error is logged; the cleanup_queue row stays where it is and
// will be retried on the next tick (still exhausted - same path).
func TestProcessCleanupQueue_Exhausted_DLQMoveFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockCleanupOps(ctrl)

	st := core.CleanupItem{ID: 7, BackendName: "b1", ObjectKey: "doomed2.txt", Attempts: 9}
	ms := &mockMetadataStore{
		pendingCleanups: []core.CleanupItem{st},
		moveDLQErr:      errors.New("db unavailable"),
	}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(nil, nil)
	ops.EXPECT().DeleteWithTimeout(gomock.Any(), gomock.Any(), "doomed2.txt").Return(errors.New("upstream timeout"))
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()

	w := NewCleanupWorker(ops, ms, 1)
	_, failed := w.ProcessCleanupQueue(context.Background())

	if failed != 1 {
		t.Errorf("expected failed=1 even on DLQ-move failure, got %d", failed)
	}
	if len(ms.dlqMoves) != 1 {
		t.Errorf("expected MoveCleanupToDLQ to be attempted once, got %d", len(ms.dlqMoves))
	}
}

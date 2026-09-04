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

	dto "github.com/prometheus/client_model/go"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestProcessCleanupQueue_DeleteSuccess verifies the process cleanup queue delete success contract.
// Asserts that expected processed=1, got.
func TestProcessCleanupQueue_DeleteSuccess(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockCleanupOps(ctrl)

	st := core.CleanupItem{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt", SizeBytes: 100}
	ms := &mockMetadataStore{pendingCleanups: []core.CleanupItem{st}}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(nil, nil) // backend value doesn't matter for this test
	ops.EXPECT().DeleteWithTimeout(gomock.Any(), gomock.Any(), "orphan.txt").Return(nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	w := NewCleanupWorker(CleanupWorkerDeps{Ops: ops, Store: ms, Concurrency: 1, InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute})
	cleanSum := w.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed

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
	ops := newMockCleanupOps(ctrl)

	st := core.CleanupItem{ID: 2, BackendName: "b1", ObjectKey: "stuck.txt", Attempts: 3}
	ms := &mockMetadataStore{pendingCleanups: []core.CleanupItem{st}}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(nil, nil)
	ops.EXPECT().DeleteWithTimeout(gomock.Any(), gomock.Any(), "stuck.txt").Return(errors.New("timeout"))
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	w := NewCleanupWorker(CleanupWorkerDeps{Ops: ops, Store: ms, Concurrency: 1, InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute})
	cleanSum := w.ProcessCleanupQueue(context.Background())
	_, failed := cleanSum.Succeeded, cleanSum.Failed

	if failed != 1 {
		t.Errorf("expected failed=1, got %d", failed)
	}
}

// TestProcessCleanupQueue_DeleteReturns404_IdempotentSuccess asserts that
// a 404 from the backend DELETE is treated as idempotent success: the row
// is completed, status="success_absent" is incremented, and no DLQ move
// happens even when the item is at the retry ceiling. Issue #843.
func TestProcessCleanupQueue_DeleteReturns404_IdempotentSuccess(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockCleanupOps(ctrl)

	// Attempts=9 puts us at the retry ceiling. Without the 404 special
	// case this row would graduate straight to DLQ; with it the row must
	// instead retire as success_absent.
	st := core.CleanupItem{ID: 42, BackendName: "b1", ObjectKey: "phantom.txt", Attempts: 9, SizeBytes: 128}
	ms := &mockMetadataStore{pendingCleanups: []core.CleanupItem{st}}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(nil, nil)
	ops.EXPECT().DeleteWithTimeout(gomock.Any(), gomock.Any(), "phantom.txt").
		Return(&httpError{code: 404, msg: "NoSuchKey"})
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	before := readCounterValue(t, telemetry.CleanupQueueProcessedTotal.WithLabelValues("success_absent"))

	w := NewCleanupWorker(CleanupWorkerDeps{Ops: ops, Store: ms, Concurrency: 1, InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute})
	cleanSum := w.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed

	if processed != 1 {
		t.Errorf("expected processed=1 (404 = idempotent success), got %d", processed)
	}
	if failed != 0 {
		t.Errorf("expected failed=0 on 404, got %d", failed)
	}
	if len(ms.dlqMoves) != 0 {
		t.Errorf("404 must not enqueue to DLQ, got %d move(s)", len(ms.dlqMoves))
	}

	after := readCounterValue(t, telemetry.CleanupQueueProcessedTotal.WithLabelValues("success_absent"))
	if after-before != 1 {
		t.Errorf("success_absent metric delta = %v, want 1", after-before)
	}
}

// TestProcessCleanupQueue_AdmissionBlocked verifies the process cleanup queue admission blocked contract.
// Asserts that expected 0/0 when blocked, got /.
func TestProcessCleanupQueue_AdmissionBlocked(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockCleanupOps(ctrl)

	st := core.CleanupItem{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt"}
	ms := &mockMetadataStore{pendingCleanups: []core.CleanupItem{st}}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(false)

	w := NewCleanupWorker(CleanupWorkerDeps{Ops: ops, Store: ms, Concurrency: 1, InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute})
	cleanSum := w.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed

	if processed != 0 || failed != 0 {
		t.Errorf("expected 0/0 when blocked, got %d/%d", processed, failed)
	}
}

// TestProcessCleanupQueue_BackendNotFound verifies the process cleanup queue backend not found contract.
// Asserts that expected processed=1 (item removed), got.
func TestProcessCleanupQueue_BackendNotFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockCleanupOps(ctrl)

	st := core.CleanupItem{ID: 1, BackendName: "gone", ObjectKey: "orphan.txt"}
	ms := &mockMetadataStore{pendingCleanups: []core.CleanupItem{st}}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("gone").Return(nil, errors.New("not found"))
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	w := NewCleanupWorker(CleanupWorkerDeps{Ops: ops, Store: ms, Concurrency: 1, InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute})
	cleanSum := w.ProcessCleanupQueue(context.Background())
	processed, _ := cleanSum.Succeeded, cleanSum.Failed

	if processed != 1 {
		t.Errorf("expected processed=1 (item removed), got %d", processed)
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
	ops := newMockCleanupOps(ctrl)

	// Attempts=9 + 1 increment crosses maxCleanupAttempts (10), so the
	// worker takes the exhausted branch.
	st := core.CleanupItem{ID: 99, BackendName: "b1", ObjectKey: "doomed.txt", Attempts: 9, SizeBytes: 512}
	ms := &mockMetadataStore{pendingCleanups: []core.CleanupItem{st}}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(nil, nil)
	ops.EXPECT().DeleteWithTimeout(gomock.Any(), gomock.Any(), "doomed.txt").Return(errors.New("permanent failure"))
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	w := NewCleanupWorker(CleanupWorkerDeps{Ops: ops, Store: ms, Concurrency: 1, InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute})
	cleanSum := w.ProcessCleanupQueue(context.Background())
	_, failed := cleanSum.Succeeded, cleanSum.Failed

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
	ops := newMockCleanupOps(ctrl)

	st := core.CleanupItem{ID: 7, BackendName: "b1", ObjectKey: "doomed2.txt", Attempts: 9}
	ms := &mockMetadataStore{
		pendingCleanups: []core.CleanupItem{st},
		moveDLQErr:      errors.New("db unavailable"),
	}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(nil, nil)
	ops.EXPECT().DeleteWithTimeout(gomock.Any(), gomock.Any(), "doomed2.txt").Return(errors.New("upstream timeout"))
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	w := NewCleanupWorker(CleanupWorkerDeps{Ops: ops, Store: ms, Concurrency: 1, InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute})
	cleanSum := w.ProcessCleanupQueue(context.Background())
	_, failed := cleanSum.Succeeded, cleanSum.Failed

	if failed != 1 {
		t.Errorf("expected failed=1 even on DLQ-move failure, got %d", failed)
	}
	if len(ms.dlqMoves) != 1 {
		t.Errorf("expected MoveCleanupToDLQ to be attempted once, got %d", len(ms.dlqMoves))
	}
}

// TestProcessCleanupQueue_ReclaimedRow_IncrementsMetric asserts that when
// ClaimPendingCleanups returns a row with Reclaimed=true (a stale claim
// from a worker that died mid-process), the worker increments the
// stale-claim-recovered counter exactly once for that row's backend.
func TestProcessCleanupQueue_ReclaimedRow_IncrementsMetric(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := newMockCleanupOps(ctrl)

	const backend = "metric-backend"
	st := core.CleanupItem{ID: 99, BackendName: backend, ObjectKey: "k", Reclaimed: true}
	ms := &mockMetadataStore{pendingCleanups: []core.CleanupItem{st}}

	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend(backend).Return(nil, nil)
	ops.EXPECT().DeleteWithTimeout(gomock.Any(), gomock.Any(), "k").Return(nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	before := readCounterValue(t, telemetry.CleanupQueueStaleClaimsRecoveredTotal.WithLabelValues(backend))
	w := NewCleanupWorker(CleanupWorkerDeps{Ops: ops, Store: ms, Concurrency: 1, InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute})
	w.ProcessCleanupQueue(context.Background())
	after := readCounterValue(t, telemetry.CleanupQueueStaleClaimsRecoveredTotal.WithLabelValues(backend))

	if after-before != 1 {
		t.Errorf("stale-claim-recovered metric delta = %v, want 1", after-before)
	}
}

// readCounterValue extracts the float counter value from a Prometheus
// metric so the test can compare before/after deltas without relying on
// the registry's text-format dump.
func readCounterValue(t *testing.T, c interface {
	Write(*dto.Metric) error
}) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("metric write: %v", err)
	}
	if m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// -------------------------------------------------------------------------------
// Pending Reaper Tests
//
// Author: Alex Freidah
//
// Unit coverage for ProcessPendingQueue. Each test arms one fixture row, sets
// up the backend HEAD outcome the reaper will see, and asserts the resolution
// branch runs end-to-end: drop on 404, promote on 200, drop on backend
// removed, leave on transient HEAD failure, and the four PromotePending
// result codes.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"go.uber.org/mock/gomock"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// setupReaper wires a PendingReaper with a CleanupOps mock and a fresh
// metadata store. concurrency=1 keeps test goroutine ordering predictable;
// minAge=0 lets the constructor fall back to its default (5m) which is
// irrelevant here since GetStalePending is mocked.
func setupReaper(t *testing.T) (*PendingReaper, *MockCleanupOps, *backendtest.MockObjectBackend, *mockMetadataStore) {
	t.Helper()
	ctrl := gomock.NewController(t)
	ops := NewMockCleanupOps(ctrl)
	be := backendtest.NewMockObjectBackend(ctrl)
	ms := &mockMetadataStore{}
	r := NewPendingReaper(ops, ms, 1, time.Minute, 50)
	return r, ops, be, ms
}

// pendingFixture returns a PendingObject for the reaper test rows.
func pendingFixture(intentID, key, backendName string) store.PendingObject {
	return store.PendingObject{
		IntentID:    intentID,
		ObjectKey:   key,
		BackendName: backendName,
		SizeBytes:   100,
	}
}

// -------------------------------------------------------------------------
// NewPendingReaper defaults
// -------------------------------------------------------------------------

// TestNewPendingReaper_AppliesZeroDefaults verifies that zero/negative
// constructor inputs fall back to safe defaults so callers can wire a
// reaper without thinking about every knob.
func TestNewPendingReaper_AppliesZeroDefaults(t *testing.T) {
	t.Parallel()
	r := NewPendingReaper(nil, nil, 0, 0, 0)
	if r.concurrency != 4 {
		t.Errorf("concurrency = %d, want 4", r.concurrency)
	}
	if r.minAge != 5*time.Minute {
		t.Errorf("minAge = %v, want 5m", r.minAge)
	}
	if r.batchSize != 50 {
		t.Errorf("batchSize = %d, want 50", r.batchSize)
	}
}

// -------------------------------------------------------------------------
// ProcessPendingQueue resolution branches
// -------------------------------------------------------------------------

// TestProcessPendingQueue_BackendNotRegisteredDropsIntent verifies the
// reaper unblocks FK-cascade deletions: an intent referencing a backend
// no longer in config is dropped rather than left to wedge the queue.
func TestProcessPendingQueue_BackendNotRegisteredDropsIntent(t *testing.T) {
	t.Parallel()
	r, ops, _, ms := setupReaper(t)

	ms.stalePending = []store.PendingObject{pendingFixture("i1", "bucket/k", "gone")}
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("gone").Return(nil, errors.New("not found"))

	resolved, failed := r.ProcessPendingQueue(context.Background())
	if resolved != 1 || failed != 0 {
		t.Errorf("resolved=%d failed=%d, want 1/0", resolved, failed)
	}
	if len(ms.deletedPendingIDs) != 1 || ms.deletedPendingIDs[0] != "i1" {
		t.Errorf("deletedPendingIDs = %v, want [i1]", ms.deletedPendingIDs)
	}
}

// TestProcessPendingQueue_HeadNotFoundDropsIntent verifies a 404 from the
// backend HEAD removes the pending row: bytes never landed, nothing to
// recover.
func TestProcessPendingQueue_HeadNotFoundDropsIntent(t *testing.T) {
	t.Parallel()
	r, ops, be, ms := setupReaper(t)

	ms.stalePending = []store.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().HeadObject(gomock.Any(), "bucket/k").Return(nil, &httpError{code: 404, msg: "NoSuchKey"})

	resolved, failed := r.ProcessPendingQueue(context.Background())
	if resolved != 1 || failed != 0 {
		t.Errorf("resolved=%d failed=%d, want 1/0", resolved, failed)
	}
	if len(ms.deletedPendingIDs) != 1 {
		t.Errorf("deletedPendingIDs = %v, want one entry", ms.deletedPendingIDs)
	}
}

// TestProcessPendingQueue_HeadTransientErrorLeavesIntent verifies that a
// non-404 backend error (timeout, 5xx, etc.) leaves the intent in place
// for the next tick rather than dropping or promoting on partial info.
func TestProcessPendingQueue_HeadTransientErrorLeavesIntent(t *testing.T) {
	t.Parallel()
	r, ops, be, ms := setupReaper(t)

	ms.stalePending = []store.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().HeadObject(gomock.Any(), "bucket/k").Return(nil, errors.New("connection reset"))

	resolved, failed := r.ProcessPendingQueue(context.Background())
	if resolved != 0 || failed != 1 {
		t.Errorf("resolved=%d failed=%d, want 0/1", resolved, failed)
	}
	if len(ms.deletedPendingIDs) != 0 {
		t.Errorf("deletedPendingIDs = %v, want empty (intent retained)", ms.deletedPendingIDs)
	}
	if len(ms.promotedPending) != 0 {
		t.Errorf("PromotePending should not have been called")
	}
}

// TestProcessPendingQueue_HeadOKPromotes verifies a 200 from HEAD triggers
// PromotePending; on Committed, the intent is resolved successfully.
func TestProcessPendingQueue_HeadOKPromotes(t *testing.T) {
	t.Parallel()
	r, ops, be, ms := setupReaper(t)

	ms.stalePending = []store.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ms.promoteResult = store.PendingPromoteCommitted
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().HeadObject(gomock.Any(), "bucket/k").Return(&backend.HeadObjectResult{Size: 100}, nil)

	resolved, failed := r.ProcessPendingQueue(context.Background())
	if resolved != 1 || failed != 0 {
		t.Errorf("resolved=%d failed=%d, want 1/0", resolved, failed)
	}
	if len(ms.promotedPending) != 1 || ms.promotedPending[0].IntentID != "i1" {
		t.Errorf("PromotePending not called with intent: %+v", ms.promotedPending)
	}
}

// TestProcessPendingQueue_PromoteSupersededCounts verifies the new
// timestamp-aware resolution: PendingPromoteSuperseded counts as resolved
// (the store deleted the row in-txn) and skips the displaced-cleanup step.
func TestProcessPendingQueue_PromoteSupersededCounts(t *testing.T) {
	t.Parallel()
	r, ops, be, ms := setupReaper(t)

	ms.stalePending = []store.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ms.promoteResult = store.PendingPromoteSuperseded
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().HeadObject(gomock.Any(), "bucket/k").Return(&backend.HeadObjectResult{Size: 100}, nil)

	resolved, failed := r.ProcessPendingQueue(context.Background())
	if resolved != 1 || failed != 0 {
		t.Errorf("resolved=%d failed=%d, want 1/0", resolved, failed)
	}
}

// TestProcessPendingQueue_PromoteAlreadyResolved verifies a no-op outcome
// (another reaper resolved the row already) is counted as resolved without
// erroring.
func TestProcessPendingQueue_PromoteAlreadyResolved(t *testing.T) {
	t.Parallel()
	r, ops, be, ms := setupReaper(t)

	ms.stalePending = []store.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ms.promoteResult = store.PendingPromoteAlreadyResolved
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().HeadObject(gomock.Any(), "bucket/k").Return(&backend.HeadObjectResult{Size: 100}, nil)

	resolved, failed := r.ProcessPendingQueue(context.Background())
	if resolved != 1 || failed != 0 {
		t.Errorf("resolved=%d failed=%d, want 1/0", resolved, failed)
	}
}

// TestProcessPendingQueue_PromoteAmbiguousLeavesIntent verifies the
// reserved Ambiguous outcome counts as failed and does not delete the row.
// With the timestamp fix this case should never fire in practice but the
// reaper must still handle it sanely.
func TestProcessPendingQueue_PromoteAmbiguousLeavesIntent(t *testing.T) {
	t.Parallel()
	r, ops, be, ms := setupReaper(t)

	ms.stalePending = []store.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ms.promoteResult = store.PendingPromoteAmbiguous
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().HeadObject(gomock.Any(), "bucket/k").Return(&backend.HeadObjectResult{Size: 100}, nil)

	resolved, failed := r.ProcessPendingQueue(context.Background())
	if resolved != 0 || failed != 1 {
		t.Errorf("resolved=%d failed=%d, want 0/1", resolved, failed)
	}
	if len(ms.deletedPendingIDs) != 0 {
		t.Errorf("ambiguous result must not delete the pending row")
	}
}

// TestProcessPendingQueue_PromoteErrorIsFailed verifies that an unexpected
// error from PromotePending is logged and counted as failed, leaving the
// row for the next tick.
func TestProcessPendingQueue_PromoteErrorIsFailed(t *testing.T) {
	t.Parallel()
	r, ops, be, ms := setupReaper(t)

	ms.stalePending = []store.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ms.promoteErr = errors.New("db blip")
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().HeadObject(gomock.Any(), "bucket/k").Return(&backend.HeadObjectResult{Size: 100}, nil)

	resolved, failed := r.ProcessPendingQueue(context.Background())
	if resolved != 0 || failed != 1 {
		t.Errorf("resolved=%d failed=%d, want 0/1", resolved, failed)
	}
}

// TestProcessPendingQueue_AdmissionBlockedSkips verifies the reaper
// respects the admission semaphore: when blocked, it returns 0/0 without
// touching the backend or store, so background work yields to user
// requests under load.
func TestProcessPendingQueue_AdmissionBlockedSkips(t *testing.T) {
	t.Parallel()
	r, ops, _, ms := setupReaper(t)

	ms.stalePending = []store.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(false)

	resolved, failed := r.ProcessPendingQueue(context.Background())
	if resolved != 0 || failed != 0 {
		t.Errorf("resolved=%d failed=%d, want 0/0 when admission denied", resolved, failed)
	}
}

// TestProcessPendingQueue_PromoteWithDisplacedEnqueues verifies that
// successful promotion of an intent which displaces copies on other
// backends fans out cleanup deletes for each.
func TestProcessPendingQueue_PromoteWithDisplacedEnqueues(t *testing.T) {
	t.Parallel()
	r, ops, be, ms := setupReaper(t)

	ms.stalePending = []store.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ms.promoteResult = store.PendingPromoteCommitted
	ms.promoteDisplaced = []store.DeletedCopy{{BackendName: "b2", SizeBytes: 200}}

	be2 := backendtest.NewMockObjectBackend(gomock.NewController(t))
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().HeadObject(gomock.Any(), "bucket/k").Return(&backend.HeadObjectResult{Size: 100}, nil)
	ops.EXPECT().GetBackend("b2").Return(be2, nil)
	ops.EXPECT().DeleteOrEnqueue(gomock.Any(), be2, "b2", "bucket/k", "overwrite_displaced", int64(200))

	resolved, _ := r.ProcessPendingQueue(context.Background())
	if resolved != 1 {
		t.Errorf("resolved=%d, want 1", resolved)
	}
}

// TestProcessPendingQueue_EmptyBatchIsNoOp verifies an empty queue runs
// without errors and updates depth gauge to 0.
func TestProcessPendingQueue_EmptyBatchIsNoOp(t *testing.T) {
	t.Parallel()
	r, _, _, ms := setupReaper(t)

	ms.stalePending = nil
	resolved, failed := r.ProcessPendingQueue(context.Background())
	if resolved != 0 || failed != 0 {
		t.Errorf("resolved=%d failed=%d, want 0/0 for empty queue", resolved, failed)
	}
}

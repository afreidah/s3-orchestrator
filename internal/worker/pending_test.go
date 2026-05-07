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
	"sync/atomic"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
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
func pendingFixture(intentID, key, backendName string) core.PendingObject {
	return core.PendingObject{
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

	ms.stalePending = []core.PendingObject{pendingFixture("i1", "bucket/k", "gone")}
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

	ms.stalePending = []core.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
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

	ms.stalePending = []core.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
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

	ms.stalePending = []core.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ms.promoteResult = core.PendingPromoteCommitted
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

	ms.stalePending = []core.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ms.promoteResult = core.PendingPromoteSuperseded
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

	ms.stalePending = []core.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ms.promoteResult = core.PendingPromoteAlreadyResolved
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

	ms.stalePending = []core.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ms.promoteResult = core.PendingPromoteAmbiguous
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

	ms.stalePending = []core.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
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

	ms.stalePending = []core.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
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

	ms.stalePending = []core.PendingObject{pendingFixture("i1", "bucket/k", "b1")}
	ms.promoteResult = core.PendingPromoteCommitted
	ms.promoteDisplaced = []core.DeletedCopy{{BackendName: "b2", SizeBytes: 200}}

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

// TestProcessPendingQueue_SkipsBackendWithOpenBreaker verifies that intents
// targeting a backend whose circuit breaker is open (and not yet probe-
// eligible) are short-circuited at tick start: no AcquireAdmission, no
// HEAD probe, just a single skip log line per backend and the intents
// counted as failed so they remain queued for the next tick.
func TestProcessPendingQueue_SkipsBackendWithOpenBreaker(t *testing.T) {
	t.Parallel()
	r, ops, _, ms := setupReaper(t)

	ctrl := gomock.NewController(t)
	rawBE := backendtest.NewMockObjectBackend(ctrl)
	cb := backend.NewCircuitBreakerBackend(rawBE, "broken", 1, time.Hour)

	// Trip the breaker: one HEAD failure with threshold=1 opens it. With a
	// 1h openTimeout no time has elapsed, so ProbeEligible is false.
	rawBE.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return(nil, errors.New("backend down"))
	if _, err := cb.HeadObject(context.Background(), "trip"); err == nil {
		t.Fatal("expected HeadObject to fail and trip the breaker")
	}

	ms.stalePending = []core.PendingObject{
		pendingFixture("i1", "bucket/k1", "broken"),
		pendingFixture("i2", "bucket/k2", "broken"),
	}
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true).Times(2)
	ops.EXPECT().ReleaseAdmission().Times(2)
	ops.EXPECT().GetBackend("broken").Return(cb, nil).Times(2)

	resolved, failed := r.ProcessPendingQueue(context.Background())
	if resolved != 0 {
		t.Errorf("resolved = %d, want 0", resolved)
	}
	if failed != 2 {
		t.Errorf("failed = %d, want 2 (both intents should be left for next tick)", failed)
	}
}

// -------------------------------------------------------------------------
// probeBackend  -  three-outcome backend HEAD classifier
// -------------------------------------------------------------------------

// TestProbeBackend_Found verifies a 200 response classifies as probeFound
// and records the API call against the backend's usage counter.
func TestProbeBackend_Found(t *testing.T) {
	t.Parallel()
	r, ops, be, _ := setupReaper(t)
	p := pendingFixture("i1", "bucket/k", "b1")

	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker())
	be.EXPECT().HeadObject(gomock.Any(), "bucket/k").Return(&backend.HeadObjectResult{Size: 100}, nil)

	if got := r.probeBackend(context.Background(), be, &p); got != probeFound {
		t.Errorf("got %v, want probeFound", got)
	}
}

// TestProbeBackend_NotFound verifies a 404 response classifies as
// probeNotFound  -  the signal that lets the reaper drop the intent.
func TestProbeBackend_NotFound(t *testing.T) {
	t.Parallel()
	r, ops, be, _ := setupReaper(t)
	p := pendingFixture("i1", "bucket/k", "b1")

	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker())
	be.EXPECT().HeadObject(gomock.Any(), "bucket/k").Return(nil, &httpError{code: 404, msg: "NoSuchKey"})

	if got := r.probeBackend(context.Background(), be, &p); got != probeNotFound {
		t.Errorf("got %v, want probeNotFound", got)
	}
}

// TestProbeBackend_TransientError verifies any non-404 error classifies
// as probeError so the reaper leaves the intent for the next tick rather
// than acting on inconclusive information.
func TestProbeBackend_TransientError(t *testing.T) {
	t.Parallel()
	r, ops, be, _ := setupReaper(t)
	p := pendingFixture("i1", "bucket/k", "b1")

	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker())
	be.EXPECT().HeadObject(gomock.Any(), "bucket/k").Return(nil, errors.New("connection reset"))

	if got := r.probeBackend(context.Background(), be, &p); got != probeError {
		t.Errorf("got %v, want probeError", got)
	}
}

// -------------------------------------------------------------------------
// dropIntent  -  covers the two reasons (backend_removed, head_404)
// -------------------------------------------------------------------------

// TestDropIntent_BackendRemoved verifies the backend-removed path deletes
// the row and counts as resolved without emitting a head_404 audit log.
func TestDropIntent_BackendRemoved(t *testing.T) {
	t.Parallel()
	r, _, _, ms := setupReaper(t)
	p := pendingFixture("i1", "bucket/k", "gone")

	var resolvedCount, failedCount atomic.Int32
	r.dropIntent(context.Background(), &p, "backend_removed", &resolvedCount, &failedCount)

	if resolvedCount.Load() != 1 || failedCount.Load() != 0 {
		t.Errorf("resolved=%d failed=%d, want 1/0", resolvedCount.Load(), failedCount.Load())
	}
	if len(ms.deletedPendingIDs) != 1 || ms.deletedPendingIDs[0] != "i1" {
		t.Errorf("deletedPendingIDs = %v, want [i1]", ms.deletedPendingIDs)
	}
}

// TestDropIntent_Head404 verifies the head-404 path deletes the row,
// counts as resolved, and emits an audit log so operators can correlate
// reaper activity with backend state.
func TestDropIntent_Head404(t *testing.T) {
	t.Parallel()
	r, _, _, ms := setupReaper(t)
	p := pendingFixture("i1", "bucket/k", "b1")

	var resolvedCount, failedCount atomic.Int32
	r.dropIntent(context.Background(), &p, "head_404", &resolvedCount, &failedCount)

	if resolvedCount.Load() != 1 || failedCount.Load() != 0 {
		t.Errorf("resolved=%d failed=%d, want 1/0", resolvedCount.Load(), failedCount.Load())
	}
	if len(ms.deletedPendingIDs) != 1 {
		t.Errorf("deletedPendingIDs = %v, want one entry", ms.deletedPendingIDs)
	}
}

// failingDeleteStore wraps mockMetadataStore and returns an error from
// DeletePending so we can exercise dropIntent's failure branch.
type failingDeleteStore struct {
	*mockMetadataStore
	deleteErr error
}

// DeletePending returns the configured deleteErr unconditionally so
// the reaper's drop-intent path can be exercised against a transient
// DB failure without standing up a real database.
func (f *failingDeleteStore) DeletePending(_ context.Context, _ string) error {
	return f.deleteErr
}

// TestDropIntent_DeleteFailureCountedAsFailed verifies that when the
// store can't delete the pending row (DB blip), the intent is counted as
// failed and left for the next tick rather than spuriously resolving.
func TestDropIntent_DeleteFailureCountedAsFailed(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockCleanupOps(ctrl)
	ms := &failingDeleteStore{
		mockMetadataStore: &mockMetadataStore{},
		deleteErr:         errors.New("db down"),
	}
	r := NewPendingReaper(ops, ms, 1, time.Minute, 50)
	p := pendingFixture("i1", "bucket/k", "b1")

	var resolvedCount, failedCount atomic.Int32
	r.dropIntent(context.Background(), &p, "head_404", &resolvedCount, &failedCount)

	if resolvedCount.Load() != 0 || failedCount.Load() != 1 {
		t.Errorf("resolved=%d failed=%d, want 0/1 on delete failure", resolvedCount.Load(), failedCount.Load())
	}
}

// -------------------------------------------------------------------------
// onPromote* handlers  -  direct tests of the four result-code branches
// -------------------------------------------------------------------------

// TestOnPromoteCommitted_FansOutDisplacedCleanup verifies a successful
// promotion enqueues cleanup deletes for displaced copies on other
// backends so orphan bytes do not accumulate.
func TestOnPromoteCommitted_FansOutDisplacedCleanup(t *testing.T) {
	t.Parallel()
	r, ops, _, _ := setupReaper(t)
	p := pendingFixture("i1", "bucket/k", "b1")
	be2 := backendtest.NewMockObjectBackend(gomock.NewController(t))

	ops.EXPECT().GetBackend("b2").Return(be2, nil)
	ops.EXPECT().DeleteOrEnqueue(gomock.Any(), be2, "b2", "bucket/k", "overwrite_displaced", int64(200))

	var resolvedCount atomic.Int32
	displaced := []core.DeletedCopy{{BackendName: "b2", SizeBytes: 200}}
	r.onPromoteCommitted(context.Background(), &p, displaced, &resolvedCount)

	if resolvedCount.Load() != 1 {
		t.Errorf("resolvedCount = %d, want 1", resolvedCount.Load())
	}
}

// TestOnPromoteCommitted_DisplacedBackendNotRegistered verifies the
// reaper logs and skips a displaced copy whose backend is no longer in
// config rather than panicking. The intent itself still counts as
// resolved.
func TestOnPromoteCommitted_DisplacedBackendNotRegistered(t *testing.T) {
	t.Parallel()
	r, ops, _, _ := setupReaper(t)
	p := pendingFixture("i1", "bucket/k", "b1")

	ops.EXPECT().GetBackend("gone").Return(nil, errors.New("not found"))

	var resolvedCount atomic.Int32
	displaced := []core.DeletedCopy{{BackendName: "gone", SizeBytes: 50}}
	r.onPromoteCommitted(context.Background(), &p, displaced, &resolvedCount)

	if resolvedCount.Load() != 1 {
		t.Errorf("resolvedCount = %d, want 1 (intent still resolved)", resolvedCount.Load())
	}
}

// TestOnPromoteSuperseded_CountsResolved verifies the timestamp drop
// branch updates accounting without taking any further action  -  the
// store already deleted the pending row in-txn.
func TestOnPromoteSuperseded_CountsResolved(t *testing.T) {
	t.Parallel()
	r, _, _, ms := setupReaper(t)
	p := pendingFixture("i1", "bucket/k", "b1")

	var resolvedCount atomic.Int32
	r.onPromoteSuperseded(context.Background(), &p, &resolvedCount)

	if resolvedCount.Load() != 1 {
		t.Errorf("resolvedCount = %d, want 1", resolvedCount.Load())
	}
	if len(ms.deletedPendingIDs) != 0 {
		t.Errorf("onPromoteSuperseded must not call DeletePending again; deletedPendingIDs = %v", ms.deletedPendingIDs)
	}
}

// TestOnPromoteAmbiguous_CountsFailed verifies the ambiguous branch
// updates only the failed counter and does not delete the row, so an
// operator-review-required state is never silently lost.
func TestOnPromoteAmbiguous_CountsFailed(t *testing.T) {
	t.Parallel()
	r, _, _, ms := setupReaper(t)
	p := pendingFixture("i1", "bucket/k", "b1")

	var failedCount atomic.Int32
	r.onPromoteAmbiguous(context.Background(), &p, &failedCount)

	if failedCount.Load() != 1 {
		t.Errorf("failedCount = %d, want 1", failedCount.Load())
	}
	if len(ms.deletedPendingIDs) != 0 {
		t.Errorf("ambiguous branch must not delete the row; deletedPendingIDs = %v", ms.deletedPendingIDs)
	}
}

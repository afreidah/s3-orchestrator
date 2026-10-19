// -------------------------------------------------------------------------------
// Cleanup Worker Fleet Tests
//
// Author: Alex Freidah
//
// Drives the cleanup worker against a real backend runtime and in-memory
// backends: claimed rows are settled through the shared settler, so a tick is
// checked by the bytes it deleted and the outcome each row was given.
// -------------------------------------------------------------------------------

package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/cleanup"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// cleanupCalls records the outcome calls a tick made on the store.
type cleanupCalls struct {
	mu       sync.Mutex
	complete []int64
	claims   []*core.CleanupClaim
	retry    []retryRecord
	dlq      []dlqRecord
}

// retryRecord is one RetryCleanupItem call: which row was rescheduled, with
// what backoff, and the error that caused it.
type retryRecord struct {
	id        int64
	backoff   time.Duration
	lastError string
}

// dlqRecord is one MoveCleanupToDLQ call: the row retired and why.
type dlqRecord struct {
	id        int64
	lastError string
}

// stuckKey is the object whose delete a test makes fail.
const stuckKey = "stuck.txt"

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// claimOnce has the store hand out items on the first claim and nothing after,
// as a queue does once its due rows are claimed.
func claimOnce(store *storetest.MockMetadataStore, items []core.CleanupItem) {
	delivered := false
	store.EXPECT().ClaimPendingCleanups(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, int, string, time.Time) ([]core.CleanupItem, error) {
			if delivered {
				return nil, nil
			}
			delivered = true
			return items, nil
		}).AnyTimes()
}

// stubProcessQueue hands out items once and records every outcome call.
func stubProcessQueue(t *testing.T, store *storetest.MockMetadataStore, calls *cleanupCalls, items []core.CleanupItem) {
	t.Helper()
	claimOnce(store, items)
	store.EXPECT().CompleteCleanupItems(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, items []core.CleanupItem) ([]int64, error) {
			calls.mu.Lock()
			defer calls.mu.Unlock()
			for i := range items {
				calls.complete = append(calls.complete, items[i].ID)
				calls.claims = append(calls.claims, items[i].Claim)
			}
			return cleanupIDs(items), nil
		}).AnyTimes()
	store.EXPECT().RetryCleanupItem(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, item *core.CleanupItem, backoff time.Duration, lastError string) (bool, error) {
			calls.mu.Lock()
			defer calls.mu.Unlock()
			calls.retry = append(calls.retry, retryRecord{id: item.ID, backoff: backoff, lastError: lastError})
			return true, nil
		}).AnyTimes()
	store.EXPECT().MoveCleanupToDLQ(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, item *core.CleanupItem, lastError string) (bool, error) {
			calls.mu.Lock()
			defer calls.mu.Unlock()
			calls.dlq = append(calls.dlq, dlqRecord{id: item.ID, lastError: lastError})
			return true, nil
		}).AnyTimes()
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestProcessCleanupQueue_DeletesTheBytesAndRetiresTheRow verifies a claimed
// row's bytes are deleted from the backend and the row is retired.
func TestProcessCleanupQueue_DeletesTheBytesAndRetiresTheRow(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "orphan.txt", bytes.NewReader([]byte("x")), 1, "", nil)

	calls := &cleanupCalls{}
	claim := &core.CleanupClaim{At: time.Now(), By: "test-instance"}
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	stubProcessQueue(t, store, calls, []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt", StorageKey: "orphan.txt", SizeBytes: 1, Claim: claim},
	})
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)
	sum := cw.ProcessCleanupQueue(context.Background())
	if sum.Succeeded != 1 || sum.Failed != 0 {
		t.Fatalf("summary = %+v, want 1 settled", sum)
	}
	if be.Has("orphan.txt") {
		t.Error("the queued bytes are still on the backend")
	}
	if len(calls.complete) != 1 || calls.complete[0] != 1 {
		t.Fatalf("completed = %v, want row 1", calls.complete)
	}
	if calls.claims[0] != claim {
		t.Errorf("completed under claim %+v, want the claim the row was handed out with", calls.claims[0])
	}
}

// TestProcessCleanupQueue_DeleteFails_SchedulesRetry verifies a failed delete
// reschedules the row with the settler's backoff and the delete's error.
func TestProcessCleanupQueue_DeleteFails_SchedulesRetry(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.DeleteErr = errors.New("be timeout")

	calls := &cleanupCalls{}
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	stubProcessQueue(t, store, calls, []core.CleanupItem{
		{ID: 2, BackendName: "b1", ObjectKey: stuckKey, StorageKey: stuckKey, Attempts: 3},
	})
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)
	sum := cw.ProcessCleanupQueue(context.Background())
	if sum.Succeeded != 0 || sum.Failed != 1 {
		t.Errorf("summary = %+v, want 1 failed", sum)
	}
	if len(calls.retry) != 1 {
		t.Fatalf("retry calls = %d, want 1", len(calls.retry))
	}
	if rc := calls.retry[0]; rc.id != 2 || rc.backoff != cleanup.Backoff(3) || rc.lastError != "be timeout" {
		t.Errorf("retry = %+v, want row 2 backed off %v with the delete error", rc, cleanup.Backoff(3))
	}
}

// TestProcessCleanupQueue_MaxAttemptsReached_MovesToDLQ verifies a row whose
// next attempt is its last graduates to the dead-letter queue instead of being
// retried.
func TestProcessCleanupQueue_MaxAttemptsReached_MovesToDLQ(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.DeleteErr = errors.New("be timeout")

	calls := &cleanupCalls{}
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	stubProcessQueue(t, store, calls, []core.CleanupItem{
		{ID: 5, BackendName: "b1", ObjectKey: stuckKey, StorageKey: stuckKey, Attempts: 9},
	})
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)
	sum := cw.ProcessCleanupQueue(context.Background())
	if sum.Failed != 1 {
		t.Errorf("failed = %d, want 1", sum.Failed)
	}
	if len(calls.dlq) != 1 || calls.dlq[0].id != 5 || calls.dlq[0].lastError != "be timeout" {
		t.Errorf("dlq moves = %+v, want row 5 with the delete error", calls.dlq)
	}
	if len(calls.retry) != 0 || len(calls.complete) != 0 {
		t.Errorf("retried %v and completed %v, want neither", calls.retry, calls.complete)
	}
}

// TestProcessCleanupQueue_BackendNotFound_RemovesItem verifies a row on a
// backend no longer configured is retired, since nothing can delete it.
func TestProcessCleanupQueue_BackendNotFound_RemovesItem(t *testing.T) {
	t.Parallel()
	calls := &cleanupCalls{}
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	stubProcessQueue(t, store, calls, []core.CleanupItem{
		{ID: 3, BackendName: "gone-backend", ObjectKey: "orphan.txt"},
	})
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)
	sum := cw.ProcessCleanupQueue(context.Background())
	if sum.Succeeded != 1 || sum.Failed != 0 {
		t.Errorf("summary = %+v, want 1 settled", sum)
	}
	if len(calls.complete) != 1 || calls.complete[0] != 3 {
		t.Errorf("completed = %v, want row 3", calls.complete)
	}
}

// TestProcessCleanupQueue_OutcomeWriteErrors verifies a failed outcome write
// is logged and does not fail the tick: a row whose retirement failed stays
// claimed and is not counted as settled, and a retried row still counts as
// failed.
func TestProcessCleanupQueue_OutcomeWriteErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		deleteErr error
		setup     func(store *storetest.MockMetadataStore)
		succeeded int
		failed    int
	}{
		{"complete fails", nil, func(store *storetest.MockMetadataStore) {
			store.EXPECT().CompleteCleanupItems(gomock.Any(), gomock.Any()).Return(nil, errors.New("db error")).AnyTimes()
		}, 0, 0},
		{"retry fails", errors.New("be down"), func(store *storetest.MockMetadataStore) {
			store.EXPECT().RetryCleanupItem(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
				Return(false, errors.New("db error on retry")).AnyTimes()
		}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			be := backendtest.NewInMemory()
			be.Objects[stuckKey] = backendtest.Object{Data: []byte("data")}
			be.DeleteErr = tc.deleteErr

			store := storetest.NewMockMetadataStore(gomock.NewController(t))
			claimOnce(store, []core.CleanupItem{{ID: 6, BackendName: "b1", ObjectKey: stuckKey, StorageKey: stuckKey, Attempts: 1}})
			tc.setup(store)
			storetest.Permissive(store)

			cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)
			sum := cw.ProcessCleanupQueue(context.Background())
			if sum.Succeeded != tc.succeeded || sum.Failed != tc.failed {
				t.Errorf("summary = %+v, want %d settled and %d failed", sum, tc.succeeded, tc.failed)
			}
		})
	}
}

// TestProcessCleanupQueue_EmptyQueue verifies a tick with nothing to claim
// settles nothing.
func TestProcessCleanupQueue_EmptyQueue(t *testing.T) {
	t.Parallel()
	store := newPermissiveStore(t)
	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if sum := cw.ProcessCleanupQueue(context.Background()); sum.Succeeded != 0 || sum.Failed != 0 {
		t.Errorf("summary = %+v, want nothing settled", sum)
	}
}

// TestProcessCleanupQueue_FetchError verifies a failed claim ends the tick
// without settling anything.
func TestProcessCleanupQueue_FetchError(t *testing.T) {
	t.Parallel()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().ClaimPendingCleanups(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)
	if sum := cw.ProcessCleanupQueue(context.Background()); sum.Succeeded != 0 || sum.Failed != 0 {
		t.Errorf("summary = %+v, want nothing settled on a claim error", sum)
	}
}

// TestProcessCleanupQueue_QueueDepthError verifies a failed depth read does not
// fail the tick, since the depths are informational.
func TestProcessCleanupQueue_QueueDepthError(t *testing.T) {
	t.Parallel()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().CleanupQueueDepth(gomock.Any()).Return(int64(0), errors.New("db error")).AnyTimes()
	store.EXPECT().CleanupDLQDepth(gomock.Any()).Return(int64(0), errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)
	if sum := cw.ProcessCleanupQueue(context.Background()); sum.Succeeded != 0 || sum.Failed != 0 {
		t.Errorf("summary = %+v, want nothing settled", sum)
	}
}

// TestProcessCleanupQueue_Concurrent verifies a batch's deletes are not run one
// at a time.
func TestProcessCleanupQueue_Concurrent(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.DeleteDelay = 50 * time.Millisecond

	var items []core.CleanupItem
	for i := range 10 {
		key := fmt.Sprintf("orphan-%d", i)
		be.Objects[key] = backendtest.Object{Data: []byte("data")}
		items = append(items, core.CleanupItem{ID: int64(i + 1), BackendName: "b1", ObjectKey: key, StorageKey: key})
	}

	calls := &cleanupCalls{}
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	stubProcessQueue(t, store, calls, items)
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)
	start := time.Now()
	sum := cw.ProcessCleanupQueue(context.Background())
	elapsed := time.Since(start)

	if sum.Succeeded != 10 || sum.Failed != 0 {
		t.Errorf("summary = %+v, want 10 settled", sum)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("elapsed = %v, want < 200ms with concurrency 10", elapsed)
	}
}

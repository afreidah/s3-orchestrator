// -------------------------------------------------------------------------------
// Cleanup Queue Manager Tests
//
// Author: Alex Freidah
//
// Tests for the cleanup retry worker: exponential backoff calculation, queue
// processing with successful and failed retries, maximum attempt enforcement,
// and best-effort enqueue behavior during database outages.
// -------------------------------------------------------------------------------

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// cleanupCalls records the calls a test wants to assert against. Each
// migrated test wires the relevant DoAndReturn closures to populate the
// slices on this struct, then reads them after exercising the system
// under test.
type cleanupCalls struct {
	mu       sync.Mutex
	enqueue  []core.CleanupItem
	complete []int64
	retry    []retryRecord
	dlq      []dlqRecord
	pending  []core.PendingObject
}

type retryRecord struct {
	id        int64
	backoff   time.Duration
	lastError string
}

type dlqRecord struct {
	id        int64
	lastError string
}

// stubEnqueue captures EnqueueCleanup calls into c.enqueue and returns
// the supplied error so tests can drive both happy and DB-outage paths.
func stubEnqueue(c *cleanupCalls, err error) func(context.Context, string, string, string, int64) error {
	return func(_ context.Context, backend, key, reason string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.enqueue = append(c.enqueue, core.CleanupItem{
			BackendName: backend, ObjectKey: key, Reason: reason, SizeBytes: size,
		})
		return err
	}
}

// TestCleanupBackoff verifies the exponential backoff schedule.
func TestCleanupBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		attempts int32
		want     time.Duration
	}{
		{0, 1 * time.Minute},
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{4, 16 * time.Minute},
		{5, 32 * time.Minute},
		{6, 64 * time.Minute},
		{7, 128 * time.Minute},
		{8, 256 * time.Minute},
		{9, 512 * time.Minute},
		{10, 1024 * time.Minute},
		{11, 24 * time.Hour},
		{15, 24 * time.Hour},
	}
	for _, tt := range tests {
		got := worker.CleanupBackoff(tt.attempts)
		if got != tt.want {
			t.Errorf("worker.CleanupBackoff(%d) = %v, want %v", tt.attempts, got, tt.want)
		}
	}
}

// TestEnqueueCleanup_Success captures a single enqueue call.
func TestEnqueueCleanup_Success(t *testing.T) {
	t.Parallel()
	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubEnqueue(calls, nil)).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	mgr.coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "orphan_put", 1024)

	if len(calls.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(calls.enqueue))
	}
	c := calls.enqueue[0]
	if c.BackendName != "b1" || c.ObjectKey != "orphan.txt" || c.Reason != "orphan_put" {
		t.Errorf("unexpected call: %+v", c)
	}
}

// TestEnqueueCleanup_DBError_LogsOnly asserts the call is recorded even
// when the store returns an error.
func TestEnqueueCleanup_DBError_LogsOnly(t *testing.T) {
	t.Parallel()
	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubEnqueue(calls, errors.New("db down"))).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	mgr.coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "orphan_put", 1024)

	if len(calls.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(calls.enqueue))
	}
}

// TestEnqueueCleanup_EnqueueFailure_RecordsMetricAndAudit pins the
// visibility contract for #805. When the cleanup_queue row itself
// fails to persist (DB outage after a successful backend write), the
// system must:
//
//   - increment s3o_cleanup_enqueue_failures_total{stage="enqueue"}
//     so operators can alert on untracked orphan risk
//   - emit a storage.OrphanEnqueueFailed audit event so operators
//     can pivot from the metric to the exact backend/key/size
//
// Without these signals, the orphan would be silent (logged-only).
func TestEnqueueCleanup_EnqueueFailure_RecordsMetricAndAudit(t *testing.T) {
	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubEnqueue(calls, errors.New("db down"))).
		AnyTimes()
	storetest.Permissive(store)
	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	var capturedEvents []string
	audit.SetOnEvent(func(event string) {
		capturedEvents = append(capturedEvents, event)
	})
	t.Cleanup(func() { audit.SetOnEvent(nil) })

	before := promtest.ToFloat64(telemetry.CleanupEnqueueFailuresTotal.WithLabelValues("b1", "orphan_put", "enqueue"))
	mgr.coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "orphan_put", 1024)
	after := promtest.ToFloat64(telemetry.CleanupEnqueueFailuresTotal.WithLabelValues("b1", "orphan_put", "enqueue"))

	if after-before != 1 {
		t.Errorf("enqueue-failure counter delta = %v, want 1", after-before)
	}
	found := slices.Contains(capturedEvents, "storage.OrphanEnqueueFailed")
	if !found {
		t.Errorf("expected storage.OrphanEnqueueFailed audit event, got %v", capturedEvents)
	}
}

// TestEnqueueCleanup_OrphanBytesFailure_RecordsMetricAndAudit covers
// the secondary failure path: the cleanup_queue row persisted but the
// orphan_bytes counter increment failed. Less severe than an enqueue
// failure (the cleanup worker will still retry the delete) but quota
// accounting drifts until reconciliation. The metric labels stage
// "orphan_bytes" so dashboards can distinguish the two failure
// shapes.
func TestEnqueueCleanup_OrphanBytesFailure_RecordsMetricAndAudit(t *testing.T) {
	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubEnqueue(calls, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(errors.New("db down")).AnyTimes()
	storetest.Permissive(store)
	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	var capturedEvents []string
	audit.SetOnEvent(func(event string) {
		capturedEvents = append(capturedEvents, event)
	})
	t.Cleanup(func() { audit.SetOnEvent(nil) })

	before := promtest.ToFloat64(telemetry.CleanupEnqueueFailuresTotal.WithLabelValues("b1", "orphan_put", "orphan_bytes"))
	mgr.coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "orphan_put", 1024)
	after := promtest.ToFloat64(telemetry.CleanupEnqueueFailuresTotal.WithLabelValues("b1", "orphan_put", "orphan_bytes"))

	if after-before != 1 {
		t.Errorf("orphan-bytes-failure counter delta = %v, want 1", after-before)
	}
	found := slices.Contains(capturedEvents, "storage.OrphanEnqueueFailed")
	if !found {
		t.Errorf("expected storage.OrphanEnqueueFailed audit event, got %v", capturedEvents)
	}
}

// stubProcessQueue wires the per-test DoAndReturn closures the cleanup
// worker needs: a pending fetch, plus complete / retry / DLQ capture.
func stubProcessQueue(t *testing.T, store *storetest.MockMetadataStore, calls *cleanupCalls, items []core.CleanupItem) {
	t.Helper()
	delivered := false
	store.EXPECT().ClaimPendingCleanups(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int, _ string, _ time.Time) ([]core.CleanupItem, error) {
			if delivered {
				return nil, nil
			}
			delivered = true
			return items, nil
		}).
		AnyTimes()
	store.EXPECT().GetPendingCleanups(gomock.Any(), gomock.Any()).
		Return(items, nil).
		AnyTimes()
	store.EXPECT().CompleteCleanupItem(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, id int64) error {
			calls.mu.Lock()
			defer calls.mu.Unlock()
			calls.complete = append(calls.complete, id)
			return nil
		}).
		AnyTimes()
	store.EXPECT().RetryCleanupItem(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, id int64, backoff time.Duration, lastError string) error {
			calls.mu.Lock()
			defer calls.mu.Unlock()
			calls.retry = append(calls.retry, retryRecord{id: id, backoff: backoff, lastError: lastError})
			return nil
		}).
		AnyTimes()
	store.EXPECT().MoveCleanupToDLQ(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, id int64, lastError string) (bool, error) {
			calls.mu.Lock()
			defer calls.mu.Unlock()
			calls.dlq = append(calls.dlq, dlqRecord{id: id, lastError: lastError})
			return true, nil
		}).
		AnyTimes()
}

// TestProcessCleanupQueue_DeleteSuccess pins the success path.
func TestProcessCleanupQueue_DeleteSuccess(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "orphan.txt", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubProcessQueue(t, store, calls, []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt", Reason: "orphan_put", Attempts: 0},
	})
	storetest.Permissive(store)

	mgr, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": backend})

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
	if processed != 1 {
		t.Errorf("expected processed=1, got %d", processed)
	}
	if failed != 0 {
		t.Errorf("expected failed=0, got %d", failed)
	}
	if len(calls.complete) != 1 || calls.complete[0] != 1 {
		t.Errorf("expected CompleteCleanupItem(1), got %v", calls.complete)
	}
	if backend.hasObject("orphan.txt") {
		t.Error("expected orphan to be deleted from backend")
	}
	if got := mgr.Runtime().Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("apiRequests = %d, want 1 (cleanup delete)", got)
	}
}

// TestProcessCleanupQueue_DeleteFails_SchedulesRetry pins the retry-on-
// transient-failure path.
func TestProcessCleanupQueue_DeleteFails_SchedulesRetry(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.delErr = errors.New("backend timeout")

	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubProcessQueue(t, store, calls, []core.CleanupItem{
		{ID: 2, BackendName: "b1", ObjectKey: "stuck.txt", Reason: "delete_failed", Attempts: 3},
	})
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": backend})

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
	if processed != 0 {
		t.Errorf("expected processed=0, got %d", processed)
	}
	if failed != 1 {
		t.Errorf("expected failed=1, got %d", failed)
	}
	if len(calls.retry) != 1 {
		t.Fatalf("expected 1 retry call, got %d", len(calls.retry))
	}
	rc := calls.retry[0]
	if rc.id != 2 {
		t.Errorf("expected retry for id=2, got %d", rc.id)
	}
	if rc.backoff != worker.CleanupBackoff(3) {
		t.Errorf("expected backoff=%v, got %v", worker.CleanupBackoff(3), rc.backoff)
	}
	if rc.lastError != "backend timeout" {
		t.Errorf("expected lastError='backend timeout', got %q", rc.lastError)
	}
}

// TestProcessCleanupQueue_BackendNotFound_RemovesItem asserts a
// gone-backend item completes immediately.
func TestProcessCleanupQueue_BackendNotFound_RemovesItem(t *testing.T) {
	t.Parallel()
	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubProcessQueue(t, store, calls, []core.CleanupItem{
		{ID: 3, BackendName: "gone-backend", ObjectKey: "orphan.txt", Reason: "orphan_put", Attempts: 0},
	})
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
	if processed != 1 {
		t.Errorf("expected processed=1, got %d", processed)
	}
	if failed != 0 {
		t.Errorf("expected failed=0, got %d", failed)
	}
	if len(calls.complete) != 1 || calls.complete[0] != 3 {
		t.Errorf("expected CompleteCleanupItem(3), got %v", calls.complete)
	}
}

// TestProcessCleanupQueue_EmptyQueue covers the empty-queue early return.
func TestProcessCleanupQueue_EmptyQueue(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
	if processed != 0 || failed != 0 {
		t.Errorf("expected 0/0, got %d/%d", processed, failed)
	}
}

// TestProcessCleanupQueue_FetchError surfaces a queue-fetch failure.
func TestProcessCleanupQueue_FetchError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ClaimPendingCleanups(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	store.EXPECT().GetPendingCleanups(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
	if processed != 0 || failed != 0 {
		t.Errorf("expected 0/0 on fetch error, got %d/%d", processed, failed)
	}
}

// TestProcessCleanupQueue_MaxAttemptsReached_MovesToDLQ asserts an
// exhausted item moves to cleanup_dlq instead of going on the retry
// path.
func TestProcessCleanupQueue_MaxAttemptsReached_MovesToDLQ(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.delErr = errors.New("backend timeout")

	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubProcessQueue(t, store, calls, []core.CleanupItem{
		{ID: 5, BackendName: "b1", ObjectKey: "stuck.txt", Reason: "delete_failed", Attempts: 9},
	})
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": backend})

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
	if processed != 0 {
		t.Errorf("expected processed=0, got %d", processed)
	}
	if failed != 1 {
		t.Errorf("expected failed=1, got %d", failed)
	}

	if len(calls.dlq) != 1 {
		t.Fatalf("expected 1 MoveCleanupToDLQ call, got %d", len(calls.dlq))
	}
	if calls.dlq[0].id != 5 {
		t.Errorf("expected MoveCleanupToDLQ(5), got id=%d", calls.dlq[0].id)
	}
	if calls.dlq[0].lastError != "backend timeout" {
		t.Errorf("expected lastError=%q, got %q", "backend timeout", calls.dlq[0].lastError)
	}
	if len(calls.retry) != 0 {
		t.Errorf("expected 0 RetryCleanupItem calls (exhausted now moves), got %d", len(calls.retry))
	}
	if len(calls.complete) != 0 {
		t.Errorf("expected 0 CompleteCleanupItem calls, got %v", calls.complete)
	}
}

// TestProcessCleanupQueue_CompleteItemError pins that a CompleteCleanupItem
// failure is logged-only.
func TestProcessCleanupQueue_CompleteItemError(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "orphan.txt", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ClaimPendingCleanups(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.CleanupItem{
			{ID: 6, BackendName: "b1", ObjectKey: "orphan.txt", Reason: "orphan_put", Attempts: 0},
		}, nil).
		AnyTimes()
	store.EXPECT().GetPendingCleanups(gomock.Any(), gomock.Any()).
		Return([]core.CleanupItem{
			{ID: 6, BackendName: "b1", ObjectKey: "orphan.txt", Reason: "orphan_put", Attempts: 0},
		}, nil).
		AnyTimes()
	store.EXPECT().CompleteCleanupItem(gomock.Any(), gomock.Any()).
		Return(errors.New("db error")).
		AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": backend})

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
	if processed != 1 {
		t.Errorf("expected processed=1 (delete succeeded), got %d", processed)
	}
	if failed != 0 {
		t.Errorf("expected failed=0, got %d", failed)
	}
}

// TestProcessCleanupQueue_RetryItemError pins that a RetryCleanupItem
// failure is logged-only.
func TestProcessCleanupQueue_RetryItemError(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.delErr = errors.New("backend down")

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ClaimPendingCleanups(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.CleanupItem{
			{ID: 7, BackendName: "b1", ObjectKey: "stuck.txt", Reason: "delete_failed", Attempts: 1},
		}, nil).
		AnyTimes()
	store.EXPECT().GetPendingCleanups(gomock.Any(), gomock.Any()).
		Return([]core.CleanupItem{
			{ID: 7, BackendName: "b1", ObjectKey: "stuck.txt", Reason: "delete_failed", Attempts: 1},
		}, nil).
		AnyTimes()
	store.EXPECT().RetryCleanupItem(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(errors.New("db error on retry")).
		AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": backend})

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
	if processed != 0 {
		t.Errorf("expected processed=0, got %d", processed)
	}
	if failed != 1 {
		t.Errorf("expected failed=1, got %d", failed)
	}
}

// TestProcessCleanupQueue_QueueDepthError ignores a CleanupQueueDepth
// failure.
func TestProcessCleanupQueue_QueueDepthError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().CleanupQueueDepth(gomock.Any()).
		Return(int64(0), errors.New("db error")).
		AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
	if processed != 0 || failed != 0 {
		t.Errorf("expected 0/0, got %d/%d", processed, failed)
	}
}

// TestProcessCleanupQueue_BackendNotFound_CompleteItemError logs the
// completion error without surfacing it.
func TestProcessCleanupQueue_BackendNotFound_CompleteItemError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ClaimPendingCleanups(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.CleanupItem{
			{ID: 8, BackendName: "gone-backend", ObjectKey: "orphan.txt", Reason: "orphan_put", Attempts: 0},
		}, nil).
		AnyTimes()
	store.EXPECT().GetPendingCleanups(gomock.Any(), gomock.Any()).
		Return([]core.CleanupItem{
			{ID: 8, BackendName: "gone-backend", ObjectKey: "orphan.txt", Reason: "orphan_put", Attempts: 0},
		}, nil).
		AnyTimes()
	store.EXPECT().CompleteCleanupItem(gomock.Any(), gomock.Any()).
		Return(errors.New("db error")).
		AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
	if processed != 1 {
		t.Errorf("expected processed=1, got %d", processed)
	}
	if failed != 0 {
		t.Errorf("expected failed=0, got %d", failed)
	}
}

// TestProcessCleanupQueue_Concurrent confirms the worker fans out items
// across its concurrency budget.
func TestProcessCleanupQueue_Concurrent(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.delDelay = 50 * time.Millisecond

	var items []core.CleanupItem
	for i := range 10 {
		key := fmt.Sprintf("orphan-%d", i)
		backend.objects[key] = mockObject{data: []byte("data")}
		items = append(items, core.CleanupItem{
			ID: int64(i + 1), BackendName: "b1", ObjectKey: key, Reason: "test", Attempts: 0,
		})
	}

	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubProcessQueue(t, store, calls, items)
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": backend})

	start := time.Now()
	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
	elapsed := time.Since(start)

	if processed != 10 {
		t.Errorf("processed = %d, want 10", processed)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("elapsed = %v, expected < 200ms with concurrency 10", elapsed)
	}
}

// TestDeleteObject_BackendDeleteFails_EnqueuesCleanup pins that a
// backend-delete failure during DeleteObject enqueues a cleanup row but
// returns nil to the caller.
func TestDeleteObject_BackendDeleteFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()

	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b1", SizeBytes: 100}}, nil).
		AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubEnqueue(calls, nil)).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	backend.mu.Lock()
	backend.delErr = errors.New("backend timeout")
	backend.mu.Unlock()

	if err := mgr.objectManager.DeleteObject(context.Background(), "mykey"); err != nil {
		t.Fatalf("DeleteObject should succeed even if backend delete fails: %v", err)
	}

	if len(calls.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(calls.enqueue))
	}
	c := calls.enqueue[0]
	if c.BackendName != "b1" || c.ObjectKey != "mykey" || c.Reason != "delete_failed" {
		t.Errorf("unexpected enqueue call: %+v", c)
	}
}

// TestProcessCleanupQueue_AdmissionBlocked confirms that a saturated
// admission semaphore stops the worker from issuing deletes.
func TestProcessCleanupQueue_AdmissionBlocked(t *testing.T) {
	t.Parallel()
	sem := make(chan struct{}, 1)
	sem <- struct{}{} // saturate

	backend := newMockBackend()
	backend.objects["orphan.txt"] = mockObject{data: []byte("data")}

	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubProcessQueue(t, store, calls, []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt", Reason: "orphan_put", Attempts: 0},
	})
	storetest.Permissive(store)

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Storage: StorageDeps{
			Backends: map[string]s3be.ObjectBackend{"b1": backend},
			Order:    []string{"b1"},
		},
		Stores: StoreDeps{
			Metadata:  testStoresFromMock(store),
			Dashboard: store,
		},
		Policies: PolicyConfig{
			CacheTTL:        5 * time.Second,
			BackendTimeout:  30 * time.Second,
			RoutingStrategy: config.RoutingPack,
		},
		Operations: OperationalDeps{
			Metrics:      store,
			AdmissionSem: sem,
		},
	})
	workers := wireWorkersForTest(mgr, store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(ctx)
	if processed != 0 {
		t.Errorf("expected processed=0 when admission blocked, got %d", processed)
	}
	if failed != 0 {
		t.Errorf("expected failed=0 when admission blocked (item skipped), got %d", failed)
	}
	if !backend.hasObject("orphan.txt") {
		t.Error("object should not be deleted when admission is blocked")
	}
}

// TestPutObject_RecordFails_DoesNotEnqueueOrphanCleanup pins the
// pending-row pattern: a record failure produces no cleanup_queue
// rows, only a pending intent.
func TestPutObject_RecordFails_DoesNotEnqueueOrphanCleanup(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()

	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b1", nil).
		AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	store.EXPECT().RecordObjectAndClearPending(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	store.EXPECT().InsertPending(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.PendingObject) error {
			calls.mu.Lock()
			defer calls.mu.Unlock()
			calls.pending = append(calls.pending, *p)
			return nil
		}).
		AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubEnqueue(calls, nil)).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	if _, err := mgr.objectManager.PutObject(context.Background(), "mykey", bytes.NewReader([]byte("data")), 4, "text/plain", nil); err == nil {
		t.Fatal("expected error from PutObject")
	}

	if len(calls.enqueue) != 0 {
		t.Fatalf("expected 0 enqueue calls (pending pattern handles recovery), got %d", len(calls.enqueue))
	}
	if len(calls.pending) != 1 {
		t.Fatalf("expected 1 InsertPending call, got %d", len(calls.pending))
	}
}

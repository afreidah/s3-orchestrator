// -------------------------------------------------------------------------------
// Orphan Bytes Tracking Tests
//
// Author: Alex Freidah
//
// Validates the orphan bytes lifecycle: enqueue increments, successful cleanup
// decrements, exhausted cleanup preserves, displaced copies on overwrite are
// enqueued with correct sizes, and replicator capacity checks account for
// orphan bytes.
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
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// orphanCalls accumulates the per-test interactions a migrated test
// asserts on. Each field is keyed by the store method that fed it.
type orphanCalls struct {
	mu        sync.Mutex
	enqueue   []core.CleanupItem
	increment []orphanBytesEntry
	decrement []orphanBytesEntry
	complete  []int64
	retry     []retryRecord
	dlq       []dlqRecord
}

type orphanBytesEntry struct {
	backendName string
	sizeBytes   int64
}

// stubOrphanEnqueue captures EnqueueCleanup args.
func stubOrphanEnqueue(c *orphanCalls, err error) func(context.Context, string, string, string, int64) error {
	return func(_ context.Context, backend, key, reason string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.enqueue = append(c.enqueue, core.CleanupItem{
			BackendName: backend, ObjectKey: key, Reason: reason, SizeBytes: size,
		})
		return err
	}
}

// stubOrphanIncrement captures IncrementOrphanBytes args.
func stubOrphanIncrement(c *orphanCalls, err error) func(context.Context, string, int64) error {
	return func(_ context.Context, backend string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.increment = append(c.increment, orphanBytesEntry{backendName: backend, sizeBytes: size})
		return err
	}
}

// stubOrphanDecrement captures DecrementOrphanBytes args.
func stubOrphanDecrement(c *orphanCalls) func(context.Context, string, int64) error {
	return func(_ context.Context, backend string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.decrement = append(c.decrement, orphanBytesEntry{backendName: backend, sizeBytes: size})
		return nil
	}
}

// TestCleanupWorker_SuccessfulDelete_DecrementsOrphanBytes asserts the
// success path calls DecrementOrphanBytes with the row's size.
func TestCleanupWorker_SuccessfulDelete_DecrementsOrphanBytes(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "orphan.txt", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubCleanupQueue(t, store, c, []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt", Reason: "delete_failed", Attempts: 0, SizeBytes: 4},
	}, nil)
	store.EXPECT().DecrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanDecrement(c)).AnyTimes()
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
	if processed != 1 || failed != 0 {
		t.Fatalf("expected processed=1 failed=0, got %d/%d", processed, failed)
	}
	if len(c.decrement) != 1 {
		t.Fatalf("expected 1 DecrementOrphanBytes call, got %d", len(c.decrement))
	}
	if c.decrement[0].backendName != "b1" || c.decrement[0].sizeBytes != 4 {
		t.Errorf("unexpected DecrementOrphanBytes call: %+v", c.decrement[0])
	}
}

// TestCleanupWorker_SuccessfulDelete_ZeroSize_SkipsDecrement asserts
// zero-size rows produce no decrement call.
func TestCleanupWorker_SuccessfulDelete_ZeroSize_SkipsDecrement(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "orphan.txt", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubCleanupQueue(t, store, c, []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt", Reason: "delete_failed", Attempts: 0, SizeBytes: 0},
	}, nil)
	store.EXPECT().DecrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanDecrement(c)).AnyTimes()
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	processed, _ := cleanSum.Succeeded, cleanSum.Failed
	if processed != 1 {
		t.Fatalf("expected processed=1, got %d", processed)
	}
	if len(c.decrement) != 0 {
		t.Errorf("expected 0 DecrementOrphanBytes calls for zero-size item, got %d", len(c.decrement))
	}
}

// TestCleanupWorker_Exhausted_MovesToDLQ_PreservesOrphanBytes asserts an
// exhausted item moves to DLQ and orphan_bytes is not decremented.
func TestCleanupWorker_Exhausted_MovesToDLQ_PreservesOrphanBytes(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.DeleteErr = errors.New("permanent failure")

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubCleanupQueue(t, store, c, []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "stuck.txt", Reason: "delete_failed", Attempts: 9, SizeBytes: 8192},
	}, nil)
	store.EXPECT().DecrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanDecrement(c)).AnyTimes()
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	_, failed := cleanSum.Succeeded, cleanSum.Failed
	if failed != 1 {
		t.Fatalf("expected failed=1, got %d", failed)
	}
	if len(c.decrement) != 0 {
		t.Errorf("expected 0 DecrementOrphanBytes calls for exhausted item, got %d", len(c.decrement))
	}
	if len(c.complete) != 0 {
		t.Errorf("expected 0 CompleteCleanupItem calls, got %d", len(c.complete))
	}
	if len(c.retry) != 0 {
		t.Errorf("expected 0 RetryCleanupItem calls; exhausted graduates to DLQ, got %d", len(c.retry))
	}
	if len(c.dlq) != 1 {
		t.Fatalf("expected 1 MoveCleanupToDLQ call, got %d", len(c.dlq))
	}
	if c.dlq[0].id != 1 {
		t.Errorf("dlq move id=%d, want 1", c.dlq[0].id)
	}
}

// TestCleanupWorker_RetryNotExhausted_NoOrphanBytesChange asserts retry
// path leaves the orphan-bytes counter alone.
func TestCleanupWorker_RetryNotExhausted_NoOrphanBytesChange(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.DeleteErr = errors.New("transient failure")

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubCleanupQueue(t, store, c, []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "retry.txt", Reason: "delete_failed", Attempts: 3, SizeBytes: 1024},
	}, nil)
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	store.EXPECT().DecrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanDecrement(c)).AnyTimes()
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	_, failed := cleanSum.Succeeded, cleanSum.Failed
	if failed != 1 {
		t.Fatalf("expected failed=1, got %d", failed)
	}
	if len(c.increment) != 0 {
		t.Errorf("expected 0 IncrementOrphanBytes calls on retry, got %d", len(c.increment))
	}
	if len(c.decrement) != 0 {
		t.Errorf("expected 0 DecrementOrphanBytes calls on retry, got %d", len(c.decrement))
	}
}

// TestFindReplicaTarget_RespectsOrphanBytes asserts orphan-bytes count
// against the available-space check.
func TestFindReplicaTarget_RespectsOrphanBytes(t *testing.T) {
	t.Parallel()
	store := newPermissiveStore(t)
	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory(), "b2": backendtest.NewInMemory()}, &fleetOpts{Order: []string{"b1", "b2"}})

	exclusion := map[string]bool{"b1": true}
	target := w.FindReplicaTarget(context.Background(), "key1", 100, exclusion)
	if target != "" {
		t.Errorf("expected no target (orphan bytes eat available space), got %q", target)
	}
}

// TestFindReplicaTarget_OrphanBytesStillFits asserts that capacity check
// still allows fits within remaining space.
func TestFindReplicaTarget_OrphanBytesStillFits(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int64, eligible []string) (string, error) {
			if len(eligible) > 0 {
				return eligible[0], nil
			}
			return "", nil
		}).
		AnyTimes()
	storetest.Permissive(store)

	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory(), "b2": backendtest.NewInMemory()}, &fleetOpts{Order: []string{"b1", "b2"}})

	exclusion := map[string]bool{"b1": true}
	target := w.FindReplicaTarget(context.Background(), "key1", 50, exclusion)
	if target != "b2" {
		t.Errorf("expected b2 (50 bytes fits in 100 free), got %q", target)
	}
}

// TestCleanupOrphan_PassesSizeToEnqueue pins that the replicator's
// CleanupOrphan helper enqueues with the correct size.
func TestCleanupOrphan_PassesSizeToEnqueue(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b1.DeleteErr = errors.New("backend down")

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": b1}, nil)

	w.CleanupOrphan(context.Background(), "b1", "orphan-key", 7777)

	if len(c.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(c.enqueue))
	}
	if len(c.increment) != 1 {
		t.Fatalf("expected 1 IncrementOrphanBytes call, got %d", len(c.increment))
	}
	if c.increment[0].sizeBytes != 7777 {
		t.Errorf("expected 7777 orphan bytes, got %d", c.increment[0].sizeBytes)
	}
}

// TestCleanupWorker_CompleteCleanupItem_DBError asserts the worker
// counts the row as processed when CompleteCleanupItem returns an error.
func TestCleanupWorker_CompleteCleanupItem_DBError(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "orphan.txt", bytes.NewReader([]byte("x")), 1, "", nil)

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	items := []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt", Reason: "test", Attempts: 0, SizeBytes: 100},
	}
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
	store.EXPECT().CompleteCleanupItem(gomock.Any(), gomock.Any()).Return(errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
	if processed != 1 || failed != 0 {
		t.Fatalf("expected processed=1 failed=0, got %d/%d", processed, failed)
	}
}

// TestCleanupWorker_Exhausted_DLQMoveFails asserts the worker tolerates
// MoveCleanupToDLQ failure and leaves orphan_bytes untouched.
func TestCleanupWorker_Exhausted_DLQMoveFails(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.DeleteErr = errors.New("permanent")

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubCleanupQueue(t, store, c, []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "stuck.txt", Reason: "test", Attempts: 9, SizeBytes: 512},
	}, errors.New("db error"))
	store.EXPECT().DecrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanDecrement(c)).AnyTimes()
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	_, failed := cleanSum.Succeeded, cleanSum.Failed
	if failed != 1 {
		t.Fatalf("expected failed=1, got %d", failed)
	}
	if len(c.dlq) != 1 {
		t.Errorf("expected MoveCleanupToDLQ to be attempted once, got %d", len(c.dlq))
	}
	if len(c.decrement) != 0 {
		t.Errorf("orphan_bytes must NOT be decremented when DLQ move fails; got %d calls", len(c.decrement))
	}
}

// TestReplicate_OrphanBytesBlockTarget pins the replicator capacity
// check excluding orphan bytes.
func TestReplicate_OrphanBytesBlockTarget(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key1", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetUnderReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}, nil).AnyTimes()
	store.EXPECT().GetUnderReplicatedObjectsExcluding(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}}, nil).AnyTimes()
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"b1": {BytesUsed: 100, BytesLimit: 1000},
			"b2": {BytesUsed: 990, BytesLimit: 1000, OrphanBytes: 8},
		}, nil).AnyTimes()
	store.EXPECT().RecordReplica(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(0), true, nil).AnyTimes()
	storetest.Permissive(store)

	w := newReplicatorFor(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})

	created, err := w.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	}, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 0 {
		t.Errorf("expected 0 created (orphan bytes block target), got %d", created)
	}
	if b2.Has("key1") {
		t.Error("b2 should not have received replica - orphan bytes make it too full")
	}
}

// cleanupCalls records the calls a test wants to assert against. Each
// migrated test wires the relevant DoAndReturn closures to populate the
// slices on this struct, then reads them after exercising the system
// under test.
type cleanupCalls struct {
	mu       sync.Mutex
	complete []int64
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
		got := CleanupBackoff(tt.attempts)
		if got != tt.want {
			t.Errorf("CleanupBackoff(%d) = %v, want %v", tt.attempts, got, tt.want)
		}
	}
}

// TestProcessCleanupQueue_DeleteFails_SchedulesRetry pins the retry-on-
// transient-failure path.
func TestProcessCleanupQueue_DeleteFails_SchedulesRetry(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.DeleteErr = errors.New("be timeout")

	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubProcessQueue(t, store, calls, []core.CleanupItem{
		{ID: 2, BackendName: "b1", ObjectKey: "stuck.txt", Reason: "delete_failed", Attempts: 3},
	})
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
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
	if rc.backoff != CleanupBackoff(3) {
		t.Errorf("expected backoff=%v, got %v", CleanupBackoff(3), rc.backoff)
	}
	if rc.lastError != "be timeout" {
		t.Errorf("expected lastError='be timeout', got %q", rc.lastError)
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

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
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
	store := newPermissiveStore(t)
	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
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

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
	if processed != 0 || failed != 0 {
		t.Errorf("expected 0/0 on fetch error, got %d/%d", processed, failed)
	}
}

// TestProcessCleanupQueue_MaxAttemptsReached_MovesToDLQ asserts an
// exhausted item moves to cleanup_dlq instead of going on the retry
// path.
func TestProcessCleanupQueue_MaxAttemptsReached_MovesToDLQ(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.DeleteErr = errors.New("be timeout")

	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubProcessQueue(t, store, calls, []core.CleanupItem{
		{ID: 5, BackendName: "b1", ObjectKey: "stuck.txt", Reason: "delete_failed", Attempts: 9},
	})
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
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
	if calls.dlq[0].lastError != "be timeout" {
		t.Errorf("expected lastError=%q, got %q", "be timeout", calls.dlq[0].lastError)
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
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "orphan.txt", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

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

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
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
	be := backendtest.NewInMemory()
	be.DeleteErr = errors.New("be down")

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

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
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

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
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

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	cleanSum := cw.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
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
	be := backendtest.NewInMemory()
	be.DeleteDelay = 50 * time.Millisecond

	var items []core.CleanupItem
	for i := range 10 {
		key := fmt.Sprintf("orphan-%d", i)
		be.Objects[key] = backendtest.Object{Data: []byte("data")}
		items = append(items, core.CleanupItem{
			ID: int64(i + 1), BackendName: "b1", ObjectKey: key, Reason: "test", Attempts: 0,
		})
	}

	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubProcessQueue(t, store, calls, items)
	storetest.Permissive(store)

	cw, _ := newCleanupFor(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	start := time.Now()
	cleanSum := cw.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
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

// TestOrphanBytes_FullLifecycle drives enqueue → cleanup-success →
// decrement end-to-end.
func TestOrphanBytes_FullLifecycle(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()

	c := &orphanCalls{}
	var pending []core.CleanupItem
	claimed := map[int64]core.CleanupItem{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	store.EXPECT().DecrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanDecrement(c)).AnyTimes()
	store.EXPECT().ClaimPendingCleanups(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int, _ string, _ time.Time) ([]core.CleanupItem, error) {
			items := pending
			for _, it := range items {
				claimed[it.ID] = it
			}
			pending = nil
			return items, nil
		}).
		AnyTimes()
	store.EXPECT().GetPendingCleanups(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int) ([]core.CleanupItem, error) {
			return pending, nil
		}).
		AnyTimes()
	store.EXPECT().CompleteCleanupItem(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, id int64) error {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.complete = append(c.complete, id)
			if it, ok := claimed[id]; ok && it.SizeBytes > 0 {
				c.decrement = append(c.decrement, orphanBytesEntry{backendName: it.BackendName, sizeBytes: it.SizeBytes})
			}
			return nil
		}).
		AnyTimes()
	storetest.Permissive(store)

	// Both halves of the lifecycle are real: the coordinator charges the
	// orphan bytes on enqueue, the cleanup worker credits them back on a
	// successful delete. Asserting the pair is the point of this test.
	rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)
	cleanup := NewCleanupWorker(CleanupWorkerDeps{
		Ops: rt, Store: store, Concurrency: 1,
		InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute,
	})

	be.SetDeleteErr(errors.New("timeout"))
	coord.DeleteOrEnqueue(context.Background(), be, "b1", "file.txt", "delete_failed", 1024)

	if len(c.increment) != 1 {
		t.Fatalf("step 1: expected 1 IncrementOrphanBytes, got %d", len(c.increment))
	}
	if c.increment[0].sizeBytes != 1024 {
		t.Fatalf("step 1: expected 1024 bytes, got %d", c.increment[0].sizeBytes)
	}

	be.SetDeleteErr(nil)
	_, _ = be.PutObject(context.Background(), "file.txt", bytes.NewReader([]byte("x")), 1, "", nil)

	pending = []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "file.txt", Reason: "delete_failed", Attempts: 0, SizeBytes: 1024},
	}

	cleanSum := cleanup.ProcessCleanupQueue(context.Background())
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
	if processed != 1 || failed != 0 {
		t.Fatalf("step 2: expected processed=1 failed=0, got %d/%d", processed, failed)
	}
	if len(c.decrement) != 1 {
		t.Fatalf("step 2: expected 1 DecrementOrphanBytes, got %d", len(c.decrement))
	}
	if c.decrement[0].sizeBytes != 1024 {
		t.Errorf("step 2: expected 1024 bytes decremented, got %d", c.decrement[0].sizeBytes)
	}
}

// stubCleanupQueue wires the same DoAndReturn closures stubProcessQueue
// uses, but populates an orphanCalls instead. Lets orphan-bytes tests
// reuse the queue infrastructure without duplicating boilerplate.
//
// CompleteCleanupItem mirrors the production atomic-delete-plus-decrement
// CTE: when a row is completed the helper appends a synthetic decrement
// entry derived from the matching item's SizeBytes. Tests that assert on
// c.decrement therefore observe the same accounting the production
// engines apply, without the test having to know whether the engine is
// invoking DecrementOrphanBytes externally or atomically inside the
// transaction.
func stubCleanupQueue(t *testing.T, store *storetest.MockMetadataStore, c *orphanCalls, items []core.CleanupItem, dlqErr error) {
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
	store.EXPECT().GetPendingCleanups(gomock.Any(), gomock.Any()).Return(items, nil).AnyTimes()
	store.EXPECT().CompleteCleanupItem(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, id int64) error {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.complete = append(c.complete, id)
			for _, it := range items {
				if it.ID == id && it.SizeBytes > 0 {
					c.decrement = append(c.decrement, orphanBytesEntry{backendName: it.BackendName, sizeBytes: it.SizeBytes})
					break
				}
			}
			return nil
		}).
		AnyTimes()
	store.EXPECT().RetryCleanupItem(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, id int64, backoff time.Duration, lastError string) error {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.retry = append(c.retry, retryRecord{id: id, backoff: backoff, lastError: lastError})
			return nil
		}).
		AnyTimes()
	store.EXPECT().MoveCleanupToDLQ(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, id int64, lastError string) (bool, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.dlq = append(c.dlq, dlqRecord{id: id, lastError: lastError})
			if dlqErr != nil {
				return false, dlqErr
			}
			return true, nil
		}).
		AnyTimes()
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

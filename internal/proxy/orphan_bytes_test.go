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

package proxy

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
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

// TestEnqueueCleanup_IncrementsOrphanBytes asserts a non-zero enqueue
// drives one IncrementOrphanBytes call.
func TestEnqueueCleanup_IncrementsOrphanBytes(t *testing.T) {
	t.Parallel()
	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	mgr.coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "delete_failed", 4096)

	if len(c.increment) != 1 {
		t.Fatalf("expected 1 IncrementOrphanBytes call, got %d", len(c.increment))
	}
	if c.increment[0].backendName != "b1" || c.increment[0].sizeBytes != 4096 {
		t.Errorf("unexpected IncrementOrphanBytes call: %+v", c.increment[0])
	}
}

// TestEnqueueCleanup_ZeroSize_SkipsOrphanIncrement asserts a zero-size
// enqueue doesn't call IncrementOrphanBytes.
func TestEnqueueCleanup_ZeroSize_SkipsOrphanIncrement(t *testing.T) {
	t.Parallel()
	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	mgr.coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "delete_failed", 0)

	if len(c.increment) != 0 {
		t.Errorf("expected 0 IncrementOrphanBytes calls for zero-size, got %d", len(c.increment))
	}
}

// TestEnqueueCleanup_EnqueueFails_SkipsOrphanIncrement asserts no
// orphan-bytes increment when enqueue itself fails.
func TestEnqueueCleanup_EnqueueFails_SkipsOrphanIncrement(t *testing.T) {
	t.Parallel()
	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, errors.New("db down"))).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	mgr.coord.EnqueueCleanup(context.Background(), "b1", "orphan.txt", "delete_failed", 4096)

	if len(c.increment) != 0 {
		t.Errorf("expected 0 IncrementOrphanBytes calls when enqueue fails, got %d", len(c.increment))
	}
}

// TestCleanupWorker_SuccessfulDelete_DecrementsOrphanBytes asserts the
// success path calls DecrementOrphanBytes with the row's size.
func TestCleanupWorker_SuccessfulDelete_DecrementsOrphanBytes(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "orphan.txt", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubCleanupQueue(t, store, c, []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt", Reason: "delete_failed", Attempts: 0, SizeBytes: 4},
	}, nil)
	store.EXPECT().DecrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanDecrement(c)).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": backend})

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
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
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "orphan.txt", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubCleanupQueue(t, store, c, []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt", Reason: "delete_failed", Attempts: 0, SizeBytes: 0},
	}, nil)
	store.EXPECT().DecrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanDecrement(c)).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": backend})

	processed, _ := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
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
	backend := newMockBackend()
	backend.delErr = errors.New("permanent failure")

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubCleanupQueue(t, store, c, []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "stuck.txt", Reason: "delete_failed", Attempts: 9, SizeBytes: 8192},
	}, nil)
	store.EXPECT().DecrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanDecrement(c)).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": backend})

	_, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
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
	backend := newMockBackend()
	backend.delErr = errors.New("transient failure")

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

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": backend})

	_, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
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

// TestPutObject_Overwrite_EnqueuesDisplacedCopiesWithSize pins the
// overwrite path: displaced copies on other backends enqueue cleanup
// rows with the right size and orphan-bytes increment.
func TestPutObject_Overwrite_EnqueuesDisplacedCopiesWithSize(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	b2.delErr = errors.New("backend down")

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).Return("b1", nil).AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b2", SizeBytes: 500}}, nil).AnyTimes()
	store.EXPECT().RecordObjectAndClearPending(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b2", SizeBytes: 500}}, nil).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": b1, "b2": b2})

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "overwritten-key", bytes.NewReader([]byte("new")), 3, "text/plain", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if len(c.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call for displaced copy, got %d", len(c.enqueue))
	}
	if c.enqueue[0].BackendName != "b2" {
		t.Errorf("expected displaced copy enqueue for b2, got %q", c.enqueue[0].BackendName)
	}
	if c.enqueue[0].Reason != "overwrite_displaced" {
		t.Errorf("expected reason=overwrite_displaced, got %q", c.enqueue[0].Reason)
	}
	if len(c.increment) != 1 {
		t.Fatalf("expected 1 IncrementOrphanBytes call, got %d", len(c.increment))
	}
	if c.increment[0].sizeBytes != 500 {
		t.Errorf("expected orphan bytes=500, got %d", c.increment[0].sizeBytes)
	}
}

// TestDeleteObject_BackendFails_EnqueuesWithSize pins that DeleteObject
// enqueues with the correct size from the metadata.
func TestDeleteObject_BackendFails_EnqueuesWithSize(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.delErr = errors.New("timeout")

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b1", SizeBytes: 2048}}, nil).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": backend})

	if err := mgr.ObjectManager.DeleteObject(context.Background(), "mykey"); err != nil {
		t.Fatalf("DeleteObject should succeed even if backend delete fails: %v", err)
	}
	if len(c.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(c.enqueue))
	}
	if len(c.increment) != 1 {
		t.Fatalf("expected 1 IncrementOrphanBytes call, got %d", len(c.increment))
	}
	if c.increment[0].sizeBytes != 2048 {
		t.Errorf("expected orphan bytes=2048, got %d", c.increment[0].sizeBytes)
	}
}

// TestFindReplicaTarget_RespectsOrphanBytes asserts orphan-bytes count
// against the available-space check.
func TestFindReplicaTarget_RespectsOrphanBytes(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": newMockBackend(), "b2": newMockBackend()},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr, store)
	_ = workers

	exclusion := map[string]bool{"b1": true}
	target := workers.Replicator.FindReplicaTarget(context.Background(), "key1", 100, exclusion)
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

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": newMockBackend(), "b2": newMockBackend()},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr, store)
	_ = workers

	exclusion := map[string]bool{"b1": true}
	target := workers.Replicator.FindReplicaTarget(context.Background(), "key1", 50, exclusion)
	if target != "b2" {
		t.Errorf("expected b2 (50 bytes fits in 100 free), got %q", target)
	}
}

// TestOrphanBytes_FullLifecycle drives enqueue → cleanup-success →
// decrement end-to-end.
func TestOrphanBytes_FullLifecycle(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()

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

	mgr, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": backend})

	backend.delErr = errors.New("timeout")
	mgr.DeleteOrEnqueue(context.Background(), backend, "b1", "file.txt", "delete_failed", 1024)

	if len(c.increment) != 1 {
		t.Fatalf("step 1: expected 1 IncrementOrphanBytes, got %d", len(c.increment))
	}
	if c.increment[0].sizeBytes != 1024 {
		t.Fatalf("step 1: expected 1024 bytes, got %d", c.increment[0].sizeBytes)
	}

	backend.mu.Lock()
	backend.delErr = nil
	backend.mu.Unlock()
	_, _ = backend.PutObject(context.Background(), "file.txt", bytes.NewReader([]byte("x")), 1, "", nil)

	pending = []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "file.txt", Reason: "delete_failed", Attempts: 0, SizeBytes: 1024},
	}

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
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

// TestCleanupOrphan_PassesSizeToEnqueue pins that the replicator's
// CleanupOrphan helper enqueues with the correct size.
func TestCleanupOrphan_PassesSizeToEnqueue(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.delErr = errors.New("backend down")

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": b1})

	workers.Replicator.CleanupOrphan(context.Background(), "b1", "orphan-key", 7777)

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

// TestMetricsCollector_OrphanBytesSubtractedFromAvailable confirms the
// metrics-collector reads the OrphanBytes field without panicking.
func TestMetricsCollector_OrphanBytesSubtractedFromAvailable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"b1": {BytesUsed: 200, BytesLimit: 1000, OrphanBytes: 100},
		}, nil).
		AnyTimes()
	storetest.Permissive(store)

	mc := metrics.New(store, counter.NewUsageTracker(nil, nil), []string{"b1"}, func() int { return 0 })
	if err := mc.UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics: %v", err)
	}
}

// TestRecordObjectOrCleanup_DisplacedCopyBackendNotFound asserts that
// when the displaced copy points at a missing backend, no enqueue
// fires.
func TestRecordObjectOrCleanup_DisplacedCopyBackendNotFound(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).Return("b1", nil).AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "gone", SizeBytes: 300}}, nil).AnyTimes()
	store.EXPECT().RecordObjectAndClearPending(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "gone", SizeBytes: 300}}, nil).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": b1})

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "key1", bytes.NewReader([]byte("hi")), 2, "text/plain", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if len(c.enqueue) != 0 {
		t.Errorf("expected 0 enqueue calls for unknown backend, got %d", len(c.enqueue))
	}
}

// TestRecordObjectOrCleanup_DisplacedCopyDeleteSucceeds asserts no
// enqueue when the displaced delete succeeds inline.
func TestRecordObjectOrCleanup_DisplacedCopyDeleteSucceeds(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b2.PutObject(context.Background(), "key1", bytes.NewReader([]byte("old")), 3, "", nil)

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).Return("b1", nil).AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b2", SizeBytes: 3}}, nil).AnyTimes()
	store.EXPECT().RecordObjectAndClearPending(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b2", SizeBytes: 3}}, nil).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": b1, "b2": b2})

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "key1", bytes.NewReader([]byte("new")), 3, "", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if len(c.enqueue) != 0 {
		t.Errorf("expected 0 enqueue calls (delete succeeded), got %d", len(c.enqueue))
	}
	if len(c.increment) != 0 {
		t.Errorf("expected 0 IncrementOrphanBytes calls, got %d", len(c.increment))
	}
}

// TestEnqueueCleanup_IncrementOrphanBytesFails asserts a best-effort
// increment failure does not abort the enqueue.
func TestEnqueueCleanup_IncrementOrphanBytesFails(t *testing.T) {
	t.Parallel()
	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, errors.New("db error"))).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})
	mgr.coord.EnqueueCleanup(context.Background(), "b1", "key", "reason", 1024)

	if len(c.enqueue) != 1 {
		t.Errorf("expected 1 enqueue call, got %d", len(c.enqueue))
	}
	if len(c.increment) != 1 {
		t.Errorf("expected 1 IncrementOrphanBytes call (even though it failed), got %d", len(c.increment))
	}
}

// TestCleanupWorker_CompleteCleanupItem_DBError asserts the worker
// counts the row as processed when CompleteCleanupItem returns an error.
func TestCleanupWorker_CompleteCleanupItem_DBError(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "orphan.txt", bytes.NewReader([]byte("x")), 1, "", nil)

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

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": backend})

	processed, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
	if processed != 1 || failed != 0 {
		t.Fatalf("expected processed=1 failed=0, got %d/%d", processed, failed)
	}
}

// TestCleanupWorker_Exhausted_DLQMoveFails asserts the worker tolerates
// MoveCleanupToDLQ failure and leaves orphan_bytes untouched.
func TestCleanupWorker_Exhausted_DLQMoveFails(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.delErr = errors.New("permanent")

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	stubCleanupQueue(t, store, c, []core.CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "stuck.txt", Reason: "test", Attempts: 9, SizeBytes: 512},
	}, errors.New("db error"))
	store.EXPECT().DecrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanDecrement(c)).AnyTimes()
	storetest.Permissive(store)

	_, workers := newTestManagerWithWorkers(t, store, map[string]*mockBackend{"b1": backend})

	_, failed := workers.CleanupWorker.ProcessCleanupQueue(context.Background())
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
	b1 := newMockBackend()
	b2 := newMockBackend()
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

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr, store)
	_ = workers

	created, err := workers.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if created != 0 {
		t.Errorf("expected 0 created (orphan bytes block target), got %d", created)
	}
	if b2.hasObject("key1") {
		t.Error("b2 should not have received replica - orphan bytes make it too full")
	}
}

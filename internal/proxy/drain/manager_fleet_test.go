// -------------------------------------------------------------------------------
// Drain Tests - Backend Purge and Drain Operations
//
// Author: Alex Freidah
//
// Unit tests for backend drain and remove operations. Validates that purge
// deletes DB records during iteration to avoid infinite loops, and that
// S3 objects are cleaned up.
// -------------------------------------------------------------------------------

package drain

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// purgedObjectSize is the per-object size every purge fixture stores, and so
// the bytes each ledger-row delete reports as freed.
const purgedObjectSize = 5

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// drainCalls captures store interactions a drain test wants to assert.
type drainCalls struct {
	mu              sync.Mutex
	deletedLocation []deleteLocationRecord
	enqueue         []core.CleanupItem
	completed       []int64
}

type deleteLocationRecord struct {
	key, backend string
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// pagedLister returns a DoAndReturn that hands out paginated
// ListObjectsByBackend results.
func pagedLister(pages [][]core.ObjectLocation, gate <-chan struct{}, err error) func(context.Context, string, int) ([]core.ObjectLocation, error) {
	idx := 0
	return func(ctx context.Context, _ string, _ int) ([]core.ObjectLocation, error) {
		if gate != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-gate:
			}
		}
		if err != nil {
			return nil, err
		}
		if idx >= len(pages) {
			return nil, nil
		}
		page := pages[idx]
		idx++
		return page, nil
	}
}

// stubDeleteObjectLocation captures DeleteObjectLocation calls, reporting size
// as the bytes the removed row freed.
func stubDeleteObjectLocation(c *drainCalls, size int64, err error) func(context.Context, string, string) (int64, error) {
	return func(_ context.Context, key, backend string) (int64, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.deletedLocation = append(c.deletedLocation, deleteLocationRecord{key: key, backend: backend})
		return size, err
	}
}

// stubDrainEnqueue captures EnqueueCleanup calls.
func stubDrainEnqueue(c *drainCalls) func(context.Context, string, string, string, int64) error {
	return func(_ context.Context, backend, key, reason string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.enqueue = append(c.enqueue, core.CleanupItem{
			BackendName: backend, ObjectKey: key, Reason: reason, SizeBytes: size,
		})
		return nil
	}
}

// stubCompleteCleanup captures CompleteCleanupItem calls.
func stubCompleteCleanup(c *drainCalls) func(context.Context, int64) error {
	return func(_ context.Context, id int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.completed = append(c.completed, id)
		return nil
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestPurgeBackendObjects_DeletesDBRecords pins the purge contract:
// every listed row is deleted from S3 and the DB.
func TestPurgeBackendObjects_DeletesDBRecords(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Objects["obj1"] = backendtest.Object{Data: []byte("data1")}
	be.Objects["obj2"] = backendtest.Object{Data: []byte("data2")}

	c := &drainCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{
				{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 5},
				{ObjectKey: "obj2", BackendName: "b1", SizeBytes: 5},
			},
			{},
		}, nil, nil)).AnyTimes()
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubDeleteObjectLocation(c, purgedObjectSize, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr, rt := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": be})

	mgr.PurgeBackendObjects(context.Background(), be, "b1", nil)

	if len(c.deletedLocation) != 2 {
		t.Fatalf("expected 2 DeleteObjectLocation calls, got %d", len(c.deletedLocation))
	}
	keys := map[string]bool{}
	for _, e := range c.deletedLocation {
		keys[e.key] = true
		if e.backend != "b1" {
			t.Errorf("DeleteObjectLocation be = %q, want b1", e.backend)
		}
	}
	if !keys["obj1"] || !keys["obj2"] {
		t.Errorf("expected obj1 and obj2 to be deleted, got %v", keys)
	}
	if be.Has("obj1") {
		t.Error("obj1 should have been deleted from S3 be")
	}
	if be.Has("obj2") {
		t.Error("obj2 should have been deleted from S3 be")
	}
	if got := rt.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 2 {
		t.Errorf("apiRequests = %d, want 2 (purge deletes)", got)
	}
}

// TestPurgeBackendObjects_EmitsProgressPerObject asserts the purge reports a
// start and an end step through the observer for each object it deletes.
func TestPurgeBackendObjects_EmitsProgressPerObject(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Objects["obj1"] = backendtest.Object{Data: []byte("data1")}
	be.Objects["obj2"] = backendtest.Object{Data: []byte("data2")}

	c := &drainCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{
				{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 5},
				{ObjectKey: "obj2", BackendName: "b1", SizeBytes: 5},
			},
			{},
		}, nil, nil)).AnyTimes()
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubDeleteObjectLocation(c, purgedObjectSize, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": be})

	var mu sync.Mutex
	starts, ends := map[string]int{}, map[string]string{}
	observer := func(s progress.Step) {
		mu.Lock()
		defer mu.Unlock()
		if s.Phase == progress.PhaseStart {
			starts[s.Label]++
		} else {
			ends[s.Label] = s.Status
		}
	}

	mgr.PurgeBackendObjects(context.Background(), be, "b1", observer)

	if starts["obj1"] != 1 || starts["obj2"] != 1 {
		t.Errorf("start steps = %v, want one each for obj1/obj2", starts)
	}
	if ends["obj1"] != progress.StatusOK || ends["obj2"] != progress.StatusOK {
		t.Errorf("end statuses = %v, want ok for obj1/obj2", ends)
	}
}

// TestPurgeBackendObjects_ContinuesOnS3DeleteFailure asserts a missing
// be object still produces the DB delete.
func TestPurgeBackendObjects_ContinuesOnS3DeleteFailure(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()

	c := &drainCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{{ObjectKey: "missing", BackendName: "b1", SizeBytes: 5}},
			{},
		}, nil, nil)).AnyTimes()
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubDeleteObjectLocation(c, purgedObjectSize, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": be})

	mgr.PurgeBackendObjects(context.Background(), be, "b1", nil)

	if len(c.deletedLocation) != 1 {
		t.Fatalf("expected 1 DeleteObjectLocation call, got %d", len(c.deletedLocation))
	}
	if c.deletedLocation[0].key != "missing" {
		t.Errorf("DeleteObjectLocation key = %q, want missing", c.deletedLocation[0].key)
	}
}

// TestPurgeBackendObjects_BailsOnZeroDBProgress pins the no-progress
// guard: when DeleteObjectLocation persistently fails on every key in
// a page, the loop bails after one iteration instead of re-listing the
// same rows forever. Without this guard a persistent DB constraint /
// partition / conflict against any row in the page would pin the
// process on the same 100 rows until restart.
func TestPurgeBackendObjects_BailsOnZeroDBProgress(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()

	// Lister returns the same non-empty page forever; if the bail
	// guard misfires, the test deadlocks (we'd loop forever calling
	// it). The atomic counter pins exact list-call count after bail.
	var listCalls atomic.Int32
	page := []core.ObjectLocation{
		{ObjectKey: "k1", BackendName: "b1", SizeBytes: 1},
		{ObjectKey: "k2", BackendName: "b1", SizeBytes: 1},
	}

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, _ int) ([]core.ObjectLocation, error) {
			listCalls.Add(1)
			return page, nil
		}).AnyTimes()
	// Every DeleteObjectLocation fails -> dbDeleted stays 0 -> bail.
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(0), errors.New("simulated persistent DB failure")).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": be})

	done := make(chan struct{})
	go func() {
		mgr.PurgeBackendObjects(context.Background(), be, "b1", nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PurgeBackendObjects deadlocked; bail-on-no-progress guard did not fire")
	}

	if got := listCalls.Load(); got != 1 {
		t.Errorf("ListObjectsByBackend called %d times, want exactly 1 (bail after first zero-progress page)", got)
	}
}

// TestRemoveBackend_PurgeTerminates pins that the purge loop exits.
func TestRemoveBackend_PurgeTerminates(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Objects["k1"] = backendtest.Object{Data: []byte("x")}

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{{ObjectKey: "k1", BackendName: "b1", SizeBytes: 1}},
			{},
		}, nil, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": be})

	done := make(chan error, 1)
	go func() {
		done <- mgr.RemoveBackend(context.Background(), "b1", true, nil)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RemoveBackend: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveBackend did not terminate within 5 seconds (infinite loop?)")
	}
}

// TestDrainOneObject_ReplicaExists_DeletesSourceWithSize asserts the
// replica-exists branch deletes only the source.
func TestDrainOneObject_ReplicaExists_DeletesSourceWithSize(t *testing.T) {
	t.Parallel()
	srcBackend := backendtest.NewInMemory()
	srcBackend.Objects["key1"] = backendtest.Object{Data: []byte("data")}

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 4},
		}, nil).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": srcBackend, "b2": backendtest.NewInMemory()})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if !mgr.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
		t.Fatal("drainOneObject should succeed when replica exists")
	}
	if srcBackend.Has("key1") {
		t.Error("source object should have been deleted")
	}
}

// TestDrainOneObject_NoCopy_MovesObjectWithSize asserts the
// no-replica branch streams the object to a new destination.
func TestDrainOneObject_NoCopy_MovesObjectWithSize(t *testing.T) {
	t.Parallel()
	srcBackend := backendtest.NewInMemory()
	srcBackend.Objects["key1"] = backendtest.Object{Data: []byte("abcd"), ContentType: "text/plain"}

	dstBackend := backendtest.NewInMemory()

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		}, nil).AnyTimes()
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(4), nil).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": srcBackend, "b2": dstBackend})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if !mgr.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
		t.Fatal("drainOneObject should succeed")
	}
	if !dstBackend.Has("key1") {
		t.Error("destination backend should have the object")
	}
}

// TestDrainOneObject_MoveLocationFails_EnqueuesOrphanWithSize asserts a
// failed MoveObjectLocation enqueues a drain_orphan cleanup row.
func TestDrainOneObject_MoveLocationFails_EnqueuesOrphanWithSize(t *testing.T) {
	t.Parallel()
	srcBackend := backendtest.NewInMemory()
	srcBackend.Objects["key1"] = backendtest.Object{Data: []byte("abcd"), ContentType: "text/plain"}

	dstBackend := backendtest.NewInMemory()

	c := &drainCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		}, nil).AnyTimes()
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(0), errors.New("serialization failure")).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubDrainEnqueue(c)).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": srcBackend, "b2": dstBackend})

	dstBackend.DeleteErr = errors.New("backend down")

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if mgr.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
		t.Fatal("drainOneObject should fail when MoveObjectLocation fails")
	}
	if len(c.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call (drain_orphan), got %d", len(c.enqueue))
	}
	if c.enqueue[0].Reason != "drain_orphan" {
		t.Errorf("reason = %q, want drain_orphan", c.enqueue[0].Reason)
	}
}

// TestDrainOneObject_StaleObject_EnqueuesOrphanWithSize asserts a 0-row
// move enqueues a drain_stale_orphan cleanup row.
func TestDrainOneObject_StaleObject_EnqueuesOrphanWithSize(t *testing.T) {
	t.Parallel()
	srcBackend := backendtest.NewInMemory()
	srcBackend.Objects["key1"] = backendtest.Object{Data: []byte("abcd"), ContentType: "text/plain"}

	dstBackend := backendtest.NewInMemory()
	dstBackend.DeleteErr = errors.New("backend down")

	c := &drainCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		}, nil).AnyTimes()
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(0), nil).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubDrainEnqueue(c)).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": srcBackend, "b2": dstBackend})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if mgr.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
		t.Fatal("drainOneObject should return false for stale object")
	}
	if len(c.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call (drain_stale_orphan), got %d", len(c.enqueue))
	}
	if c.enqueue[0].Reason != "drain_stale_orphan" {
		t.Errorf("reason = %q, want drain_stale_orphan", c.enqueue[0].Reason)
	}
}

// TestStartDrain_FlushesCleanupQueueBeforeDeleteBackendData pins that
// drain processes pending cleanup queue items first.
func TestStartDrain_FlushesCleanupQueueBeforeDeleteBackendData(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Objects["orphan"] = backendtest.Object{Data: []byte("stale")}

	c := &drainCalls{}
	pending := []core.CleanupItem{
		{ID: 42, BackendName: "b1", ObjectKey: "orphan", Attempts: 0},
	}
	delivered := false
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
	store.EXPECT().ClaimPendingCleanups(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int, _ string, _ time.Time) ([]core.CleanupItem, error) {
			if delivered {
				return nil, nil
			}
			delivered = true
			return pending, nil
		}).AnyTimes()
	store.EXPECT().GetPendingCleanups(gomock.Any(), gomock.Any()).Return(pending, nil).AnyTimes()
	store.EXPECT().CompleteCleanupItem(gomock.Any(), gomock.Any()).
		DoAndReturn(stubCompleteCleanup(c)).AnyTimes()
	storetest.Permissive(store)

	// The queue flush is the behaviour under test, so this fixture runs a real
	// cleanup worker rather than the inert callback.
	var cleanup *worker.CleanupWorker
	mgr, rt := newDrainFleetWithCleanup(t, store, map[string]backend.ObjectBackend{"b1": be},
		func(ctx context.Context) (int, int) {
			sum := cleanup.ProcessCleanupQueue(ctx)
			return sum.Succeeded, sum.Failed
		})
	cleanup = worker.NewCleanupWorker(worker.CleanupWorkerDeps{
		Ops: rt, Store: store, Concurrency: 1,
		InstanceID: "drain-test", ClaimGracePeriod: 5 * time.Minute,
	})

	if err := mgr.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	testx.Eventually(t, 3*time.Second, func() bool {
		p, pErr := mgr.GetDrainProgress(context.Background(), "b1")
		return pErr == nil && !p.Active
	}, "drain did not complete within timeout")
	progress, err := mgr.GetDrainProgress(context.Background(), "b1")
	if err != nil {
		t.Fatalf("GetDrainProgress: %v", err)
	}
	if progress.Error != "" {
		t.Fatalf("drain completed with error: %s", progress.Error)
	}

	if !slices.Contains(c.completed, 42) {
		t.Error("expected cleanup item 42 to be completed during drain, but it was not")
	}
	if be.Has("orphan") {
		t.Error("orphaned object should have been deleted from S3 be")
	}
}

// TestCancelDrain_CompletedDrain_ClearsState asserts an idempotent
// cancel on a completed drain clears state.
func TestCancelDrain_CompletedDrain_ClearsState(t *testing.T) {
	t.Parallel()
	store := newPermissiveStore(t)
	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()})

	if err := mgr.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	testx.Eventually(t, 3*time.Second, func() bool {
		p, err := mgr.GetDrainProgress(context.Background(), "b1")
		return err == nil && !p.Active
	}, "drain did not complete")

	if err := mgr.CancelDrain("b1"); err != nil {
		t.Fatalf("CancelDrain: %v", err)
	}

	if err := mgr.CancelDrain("b1"); err == nil {
		t.Error("expected error after clearing drained state")
	}
}

// TestDrainActive_NetZero_AfterCompletion pins issue #883: when a drain
// completes successfully, the s3o_drain_active gauge must return to its
// pre-drain value. Before the sync.Once fix, calling CancelDrain on a
// drain that had already finalized double-decremented the gauge and
// could drive it negative.
func TestDrainActive_NetZero_AfterCompletion(t *testing.T) {
	store := newPermissiveStore(t)
	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()})

	before := promtest.ToFloat64(telemetry.DrainActive)

	if err := mgr.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	testx.Eventually(t, 3*time.Second, func() bool {
		p, err := mgr.GetDrainProgress(context.Background(), "b1")
		return err == nil && !p.Active
	}, "drain did not complete")

	// CancelDrain after natural completion would, pre-fix, decrement
	// a second time. Post-fix sync.Once on drainState makes it a no-op.
	if err := mgr.CancelDrain("b1"); err != nil {
		t.Fatalf("CancelDrain: %v", err)
	}

	if got := promtest.ToFloat64(telemetry.DrainActive); got != before {
		t.Errorf("DrainActive = %v, want %v (net change must be zero)", got, before)
	}
}

// TestDrainActive_NetZero_AfterCancelActive pins the gauge invariant
// for the cancel-during-active path: the drain goroutine bails via
// ctx.Err(), abortDrainWithError decrements once, and CancelDrain's
// post-wake call is a no-op via sync.Once.
func TestDrainActive_NetZero_AfterCancelActive(t *testing.T) {
	gate := make(chan struct{})
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister(nil, gate, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()})

	before := promtest.ToFloat64(telemetry.DrainActive)

	if err := mgr.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // let drain block on the gated lister
	if err := mgr.CancelDrain("b1"); err != nil {
		t.Fatalf("CancelDrain: %v", err)
	}

	if got := promtest.ToFloat64(telemetry.DrainActive); got != before {
		t.Errorf("DrainActive = %v, want %v (net change must be zero)", got, before)
	}
}

// TestDrainActive_NetZero_AfterDrainError pins the self-abort path:
// when the drain goroutine errors out (ListObjectsByBackend failure
// here), abortDrainWithError decrements the gauge exactly once. No
// CancelDrain follow-up - abortDrainWithError already deletes the
// state from d.draining so a subsequent CancelDrain returns "not
// draining" rather than racing.
func TestDrainActive_NetZero_AfterDrainError(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db connection lost")).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()})

	before := promtest.ToFloat64(telemetry.DrainActive)

	if err := mgr.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	testx.Eventually(t, 3*time.Second, func() bool {
		p, _ := mgr.GetDrainProgress(context.Background(), "b1")
		return !p.Active
	}, "drain did not abort")

	if got := promtest.ToFloat64(telemetry.DrainActive); got != before {
		t.Errorf("DrainActive = %v, want %v (net change must be zero)", got, before)
	}
}

// TestCancelDrain_ActiveDrain_CancelsAndClears asserts cancelling an
// in-progress drain unblocks via context.
func TestCancelDrain_ActiveDrain_CancelsAndClears(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister(nil, gate, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()})

	if err := mgr.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	time.Sleep(20 * time.Millisecond)

	if err := mgr.CancelDrain("b1"); err != nil {
		t.Fatalf("CancelDrain: %v", err)
	}
}

// TestGetDrainProgress_ConcurrentAccess hammers GetDrainProgress while
// drain is active; race detector validates the test.
func TestGetDrainProgress_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister(nil, gate, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()})

	if err := mgr.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	done := make(chan struct{})
	for range 10 {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					_, _ = mgr.GetDrainProgress(context.Background(), "b1")
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(done)
	_ = mgr.CancelDrain("b1")
}

// TestGetDrainProgress_ReportsError surfaces a DeleteBackendData error.
func TestGetDrainProgress_ReportsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().DeleteBackendData(gomock.Any(), gomock.Any()).
		Return(errors.New("injected DB failure")).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()})

	if err := mgr.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	var progress *Progress
	testx.Eventually(t, 3*time.Second, func() bool {
		p, _ := mgr.GetDrainProgress(context.Background(), "b1")
		if !p.Active {
			progress = p
			return true
		}
		return false
	}, "drain did not terminate")
	if progress == nil {
		t.Fatal("drain did not terminate")
	}
	if progress.Error == "" {
		t.Error("expected error in progress after DeleteBackendData failure")
	}
}

// TestRunDrain_ListObjectsByBackendFails surfaces a list-objects error
// path.
func TestRunDrain_ListObjectsByBackendFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db connection lost")).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()})

	if err := mgr.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	testx.Eventually(t, 3*time.Second, func() bool {
		p, _ := mgr.GetDrainProgress(context.Background(), "b1")
		return !p.Active
	}, "drain remained active after error")

	p, _ := mgr.GetDrainProgress(context.Background(), "b1")
	if p.Active {
		t.Error("drain should have terminated after ListObjectsByBackend failure")
	}
}

// TestRunDrain_DeleteBackendDataFails surfaces a backend-data delete
// failure.
func TestRunDrain_DeleteBackendDataFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
	store.EXPECT().DeleteBackendData(gomock.Any(), gomock.Any()).
		Return(errors.New("db write failed")).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()})

	if err := mgr.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	testx.Eventually(t, 3*time.Second, func() bool {
		p, err := mgr.GetDrainProgress(context.Background(), "b1")
		return err != nil || !p.Active
	}, "drain did not complete")

	p, _ := mgr.GetDrainProgress(context.Background(), "b1")
	if p == nil || p.Error == "" {
		t.Error("expected drain error from DeleteBackendData failure")
	}
}

// TestDrainOneObject_GetAllLocationsFails handles a metadata-side
// failure in DrainOneObject.
func TestDrainOneObject_GetAllLocationsFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if mgr.DrainOneObject(context.Background(), backendtest.NewInMemory(), "b1", obj) {
		t.Error("expected failure when GetAllObjectLocations fails")
	}
}

// TestDrainOneObject_DeleteSourceLocationFails handles a delete failure.
func TestDrainOneObject_DeleteSourceLocationFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 4},
		}, nil).AnyTimes()
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(0), errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	srcBackend := backendtest.NewInMemory()
	srcBackend.Objects["key1"] = backendtest.Object{Data: []byte("data")}

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": srcBackend, "b2": backendtest.NewInMemory()})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if mgr.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
		t.Error("expected failure when DeleteObjectLocation fails")
	}
}

// TestDrainOneObject_NoDestinationAvailable handles a missing dest.
func TestDrainOneObject_NoDestinationAvailable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		}, nil).AnyTimes()
	storetest.Permissive(store)

	// The draining backend is the only one in the fleet, so there is nowhere to
	// move its objects to.
	srcBackend := backendtest.NewInMemory()
	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": srcBackend})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if mgr.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
		t.Error("expected failure when no destination available")
	}
}

// TestDrainOneObject_StreamCopyFails handles a backend Get failure.
func TestDrainOneObject_StreamCopyFails(t *testing.T) {
	t.Parallel()
	srcBackend := backendtest.NewInMemory()
	srcBackend.GetErr = errors.New("read failure")

	dstBackend := backendtest.NewInMemory()

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		}, nil).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": srcBackend, "b2": dstBackend})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if mgr.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
		t.Error("expected failure when streamCopy fails")
	}
}

// TestPurgeBackendObjects_ListObjectsFails returns early on a list
// failure.
func TestPurgeBackendObjects_ListObjectsFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()})

	mgr.PurgeBackendObjects(context.Background(), backendtest.NewInMemory(), "b1", nil)
}

// TestPurgeBackendObjects_S3DeleteFails_LogsWarning ensures the DB
// delete fires even on a be delete failure.
func TestPurgeBackendObjects_S3DeleteFails_LogsWarning(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.DeleteErr = errors.New("s3 timeout")

	c := &drainCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 5}},
			{},
		}, nil, nil)).AnyTimes()
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubDeleteObjectLocation(c, purgedObjectSize, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": be})

	mgr.PurgeBackendObjects(context.Background(), be, "b1", nil)

	if len(c.deletedLocation) != 1 {
		t.Fatalf("expected 1 DeleteObjectLocation call, got %d", len(c.deletedLocation))
	}
}

// TestPurgeBackendObjects_DBDeleteFails tolerates DB failures.
func TestPurgeBackendObjects_DBDeleteFails(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Objects["obj1"] = backendtest.Object{Data: []byte("data")}

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 5}},
			{},
		}, nil, nil)).AnyTimes()
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(0), errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": be})

	mgr.PurgeBackendObjects(context.Background(), be, "b1", nil)
}

// _ = s3be ensures the import stays in scope for the type-alias usage.
var _ = func() backend.ObjectBackend { return nil }

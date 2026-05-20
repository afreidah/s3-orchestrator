// -------------------------------------------------------------------------------
// Drain Tests - Backend Purge and Drain Operations
//
// Author: Alex Freidah
//
// Unit tests for backend drain and remove operations. Validates that purge
// deletes DB records during iteration to avoid infinite loops, and that
// S3 objects are cleaned up.
// -------------------------------------------------------------------------------

package proxy

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

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"
)

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

// stubDeleteObjectLocation captures DeleteObjectLocation calls.
func stubDeleteObjectLocation(c *drainCalls, err error) func(context.Context, string, string) error {
	return func(_ context.Context, key, backend string) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.deletedLocation = append(c.deletedLocation, deleteLocationRecord{key: key, backend: backend})
		return err
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

// newDrainTestManager constructs a manager wired for drain tests.
func newDrainTestManager(t *testing.T, store core.MetadataStore, backends map[string]*mockBackend) *BackendManager {
	t.Helper()
	obs := make(map[string]s3be.ObjectBackend, len(backends))
	var order []string
	for name, b := range backends {
		obs[name] = b
		order = append(order, name)
	}
	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           order,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := wireWorkersForTest(mgr)
	_ = workers
	return mgr
}

// TestPurgeBackendObjects_DeletesDBRecords pins the purge contract:
// every listed row is deleted from S3 and the DB.
func TestPurgeBackendObjects_DeletesDBRecords(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.objects["obj1"] = mockObject{data: []byte("data1")}
	backend.objects["obj2"] = mockObject{data: []byte("data2")}

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
		DoAndReturn(stubDeleteObjectLocation(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": backend})

	mgr.DrainManager.PurgeBackendObjects(context.Background(), backend, "b1")

	if len(c.deletedLocation) != 2 {
		t.Fatalf("expected 2 DeleteObjectLocation calls, got %d", len(c.deletedLocation))
	}
	keys := map[string]bool{}
	for _, e := range c.deletedLocation {
		keys[e.key] = true
		if e.backend != "b1" {
			t.Errorf("DeleteObjectLocation backend = %q, want b1", e.backend)
		}
	}
	if !keys["obj1"] || !keys["obj2"] {
		t.Errorf("expected obj1 and obj2 to be deleted, got %v", keys)
	}
	if backend.hasObject("obj1") {
		t.Error("obj1 should have been deleted from S3 backend")
	}
	if backend.hasObject("obj2") {
		t.Error("obj2 should have been deleted from S3 backend")
	}
	if got := mgr.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 2 {
		t.Errorf("apiRequests = %d, want 2 (purge deletes)", got)
	}
}

// TestPurgeBackendObjects_ContinuesOnS3DeleteFailure asserts a missing
// backend object still produces the DB delete.
func TestPurgeBackendObjects_ContinuesOnS3DeleteFailure(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()

	c := &drainCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{{ObjectKey: "missing", BackendName: "b1", SizeBytes: 5}},
			{},
		}, nil, nil)).AnyTimes()
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubDeleteObjectLocation(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": backend})

	mgr.DrainManager.PurgeBackendObjects(context.Background(), backend, "b1")

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
	backend := newMockBackend()

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
		Return(errors.New("simulated persistent DB failure")).AnyTimes()
	storetest.Permissive(store)

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": backend})

	done := make(chan struct{})
	go func() {
		mgr.DrainManager.PurgeBackendObjects(context.Background(), backend, "b1")
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
	backend := newMockBackend()
	backend.objects["k1"] = mockObject{data: []byte("x")}

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{{ObjectKey: "k1", BackendName: "b1", SizeBytes: 1}},
			{},
		}, nil, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": backend})

	done := make(chan error, 1)
	go func() {
		done <- mgr.DrainManager.RemoveBackend(context.Background(), "b1", true)
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
	srcBackend := newMockBackend()
	srcBackend.objects["key1"] = mockObject{data: []byte("data")}

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 4},
		}, nil).AnyTimes()
	storetest.Permissive(store)

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": srcBackend, "b2": newMockBackend()})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if !mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
		t.Fatal("drainOneObject should succeed when replica exists")
	}
	if srcBackend.hasObject("key1") {
		t.Error("source object should have been deleted")
	}
}

// TestDrainOneObject_NoCopy_MovesObjectWithSize asserts the
// no-replica branch streams the object to a new destination.
func TestDrainOneObject_NoCopy_MovesObjectWithSize(t *testing.T) {
	t.Parallel()
	srcBackend := newMockBackend()
	srcBackend.objects["key1"] = mockObject{data: []byte("abcd"), contentType: "text/plain"}

	dstBackend := newMockBackend()

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		}, nil).AnyTimes()
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b2", nil).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b2", nil).AnyTimes()
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(4), nil).AnyTimes()
	storetest.Permissive(store)

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": srcBackend, "b2": dstBackend})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if !mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
		t.Fatal("drainOneObject should succeed")
	}
	if !dstBackend.hasObject("key1") {
		t.Error("destination backend should have the object")
	}
}

// TestDrainOneObject_MoveLocationFails_EnqueuesOrphanWithSize asserts a
// failed MoveObjectLocation enqueues a drain_orphan cleanup row.
func TestDrainOneObject_MoveLocationFails_EnqueuesOrphanWithSize(t *testing.T) {
	t.Parallel()
	srcBackend := newMockBackend()
	srcBackend.objects["key1"] = mockObject{data: []byte("abcd"), contentType: "text/plain"}

	dstBackend := newMockBackend()

	c := &drainCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		}, nil).AnyTimes()
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b2", nil).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b2", nil).AnyTimes()
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(0), errors.New("serialization failure")).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubDrainEnqueue(c)).AnyTimes()
	storetest.Permissive(store)

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": srcBackend, "b2": dstBackend})

	dstBackend.delErr = errors.New("backend down")

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
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
	srcBackend := newMockBackend()
	srcBackend.objects["key1"] = mockObject{data: []byte("abcd"), contentType: "text/plain"}

	dstBackend := newMockBackend()
	dstBackend.delErr = errors.New("backend down")

	c := &drainCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		}, nil).AnyTimes()
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b2", nil).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b2", nil).AnyTimes()
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(0), nil).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubDrainEnqueue(c)).AnyTimes()
	storetest.Permissive(store)

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": srcBackend, "b2": dstBackend})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
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
	backend := newMockBackend()
	backend.objects["orphan"] = mockObject{data: []byte("stale")}

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

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": backend})

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	testx.Eventually(t, 3*time.Second, func() bool {
		p, pErr := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
		return pErr == nil && !p.Active
	}, "drain did not complete within timeout")
	progress, err := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
	if err != nil {
		t.Fatalf("GetDrainProgress: %v", err)
	}
	if progress.Error != "" {
		t.Fatalf("drain completed with error: %s", progress.Error)
	}

	if !slices.Contains(c.completed, 42) {
		t.Error("expected cleanup item 42 to be completed during drain, but it was not")
	}
	if backend.hasObject("orphan") {
		t.Error("orphaned object should have been deleted from S3 backend")
	}
}

// TestCancelDrain_CompletedDrain_ClearsState asserts an idempotent
// cancel on a completed drain clears state.
func TestCancelDrain_CompletedDrain_ClearsState(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	testx.Eventually(t, 3*time.Second, func() bool {
		p, err := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
		return err == nil && !p.Active
	}, "drain did not complete")

	if err := mgr.DrainManager.CancelDrain("b1"); err != nil {
		t.Fatalf("CancelDrain: %v", err)
	}

	if err := mgr.DrainManager.CancelDrain("b1"); err == nil {
		t.Error("expected error after clearing drained state")
	}
}

// TestDrainActive_NetZero_AfterCompletion pins issue #883: when a drain
// completes successfully, the s3o_drain_active gauge must return to its
// pre-drain value. Before the sync.Once fix, calling CancelDrain on a
// drain that had already finalized double-decremented the gauge and
// could drive it negative.
func TestDrainActive_NetZero_AfterCompletion(t *testing.T) {
	store := newPermissiveMock(t)
	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	before := promtest.ToFloat64(telemetry.DrainActive)

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	testx.Eventually(t, 3*time.Second, func() bool {
		p, err := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
		return err == nil && !p.Active
	}, "drain did not complete")

	// CancelDrain after natural completion would, pre-fix, decrement
	// a second time. Post-fix sync.Once on drainState makes it a no-op.
	if err := mgr.DrainManager.CancelDrain("b1"); err != nil {
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

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	before := promtest.ToFloat64(telemetry.DrainActive)

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // let drain block on the gated lister
	if err := mgr.DrainManager.CancelDrain("b1"); err != nil {
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

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	before := promtest.ToFloat64(telemetry.DrainActive)

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	testx.Eventually(t, 3*time.Second, func() bool {
		p, _ := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
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

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	time.Sleep(20 * time.Millisecond)

	if err := mgr.DrainManager.CancelDrain("b1"); err != nil {
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

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
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
					_, _ = mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(done)
	_ = mgr.DrainManager.CancelDrain("b1")
}

// TestGetDrainProgress_ReportsError surfaces a DeleteBackendData error.
func TestGetDrainProgress_ReportsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().DeleteBackendData(gomock.Any(), gomock.Any()).
		Return(errors.New("injected DB failure")).AnyTimes()
	storetest.Permissive(store)

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	var progress *drain.Progress
	testx.Eventually(t, 3*time.Second, func() bool {
		p, _ := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
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

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	testx.Eventually(t, 3*time.Second, func() bool {
		p, _ := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
		return !p.Active
	}, "drain remained active after error")

	p, _ := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
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

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	testx.Eventually(t, 3*time.Second, func() bool {
		p, err := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
		return err != nil || !p.Active
	}, "drain did not complete")

	p, _ := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
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

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if mgr.DrainManager.DrainOneObject(context.Background(), newMockBackend(), "b1", obj) {
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
		Return(errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	srcBackend := newMockBackend()
	srcBackend.objects["key1"] = mockObject{data: []byte("data")}

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": srcBackend, "b2": newMockBackend()})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
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
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("", core.ErrNoSpaceAvailable).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("", core.ErrNoSpaceAvailable).AnyTimes()
	storetest.Permissive(store)

	srcBackend := newMockBackend()
	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": srcBackend})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
		t.Error("expected failure when no destination available")
	}
}

// TestDrainOneObject_DestBackendNotFound handles an unknown destination.
func TestDrainOneObject_DestBackendNotFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		}, nil).AnyTimes()
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("ghost", nil).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("ghost", nil).AnyTimes()
	storetest.Permissive(store)

	srcBackend := newMockBackend()
	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": srcBackend})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
		t.Error("expected failure when destination backend not found")
	}
}

// TestDrainOneObject_StreamCopyFails handles a backend Get failure.
func TestDrainOneObject_StreamCopyFails(t *testing.T) {
	t.Parallel()
	srcBackend := newMockBackend()
	srcBackend.getErr = errors.New("read failure")

	dstBackend := newMockBackend()

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		}, nil).AnyTimes()
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b2", nil).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b2", nil).AnyTimes()
	storetest.Permissive(store)

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": srcBackend, "b2": dstBackend})

	obj := &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	if mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj) {
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

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	mgr.DrainManager.PurgeBackendObjects(context.Background(), newMockBackend(), "b1")
}

// TestPurgeBackendObjects_S3DeleteFails_LogsWarning ensures the DB
// delete fires even on a backend delete failure.
func TestPurgeBackendObjects_S3DeleteFails_LogsWarning(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.delErr = errors.New("s3 timeout")

	c := &drainCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 5}},
			{},
		}, nil, nil)).AnyTimes()
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubDeleteObjectLocation(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": backend})

	mgr.DrainManager.PurgeBackendObjects(context.Background(), backend, "b1")

	if len(c.deletedLocation) != 1 {
		t.Fatalf("expected 1 DeleteObjectLocation call, got %d", len(c.deletedLocation))
	}
}

// TestPurgeBackendObjects_DBDeleteFails tolerates DB failures.
func TestPurgeBackendObjects_DBDeleteFails(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.objects["obj1"] = mockObject{data: []byte("data")}

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 5}},
			{},
		}, nil, nil)).AnyTimes()
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr := newDrainTestManager(t, store, map[string]*mockBackend{"b1": backend})

	mgr.DrainManager.PurgeBackendObjects(context.Background(), backend, "b1")
}

// _ = s3be ensures the import stays in scope for the type-alias usage.
var _ = func() s3be.ObjectBackend { return nil }

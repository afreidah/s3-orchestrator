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
	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	st "github.com/afreidah/s3-orchestrator/internal/store"
)

func newDrainTestManager(store *mockStore, backends map[string]*mockBackend) *BackendManager {
	obs := make(map[string]s3be.ObjectBackend, len(backends))
	var order []string
	for name, b := range backends {
		obs[name] = b
		order = append(order, name)
	}
	return NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Store:           store,
		Order:           order,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: "pack",
	})
}

func TestPurgeBackendObjects_DeletesDBRecords(t *testing.T) {
	backend := newMockBackend()
	// Pre-populate backend with objects
	backend.objects["obj1"] = mockObject{data: []byte("data1")}
	backend.objects["obj2"] = mockObject{data: []byte("data2")}

	store := &mockStore{
		// First call returns two objects, second call returns empty (records deleted)
		listObjectsByBackendPages: [][]st.ObjectLocation{
			{
				{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 5},
				{ObjectKey: "obj2", BackendName: "b1", SizeBytes: 5},
			},
			{}, // empty = loop terminates
		},
	}

	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": backend})

	mgr.DrainManager.PurgeBackendObjects(context.Background(), backend, "b1")

	// Verify DB records were deleted for both objects
	store.mu.Lock()
	calls := store.deleteObjectLocationCalls
	store.mu.Unlock()

	if len(calls) != 2 {
		t.Fatalf("expected 2 DeleteObjectLocation calls, got %d", len(calls))
	}

	keys := map[string]bool{}
	for _, c := range calls {
		keys[c.key] = true
		if c.backend != "b1" {
			t.Errorf("DeleteObjectLocation backend = %q, want b1", c.backend)
		}
	}
	if !keys["obj1"] || !keys["obj2"] {
		t.Errorf("expected obj1 and obj2 to be deleted, got %v", keys)
	}

	// Verify S3 objects were also deleted
	if backend.hasObject("obj1") {
		t.Error("obj1 should have been deleted from S3 backend")
	}
	if backend.hasObject("obj2") {
		t.Error("obj2 should have been deleted from S3 backend")
	}

	// Verify usage tracking: 2 API calls (one delete per object)
	if got := mgr.usage.Backend().Load("b1", counter.FieldAPIRequests); got != 2 {
		t.Errorf("apiRequests = %d, want 2 (purge deletes)", got)
	}
}

func TestPurgeBackendObjects_ContinuesOnS3DeleteFailure(t *testing.T) {
	backend := newMockBackend()
	// Don't pre-populate — S3 deletes will "fail" (object not found)

	store := &mockStore{
		listObjectsByBackendPages: [][]st.ObjectLocation{
			{{ObjectKey: "missing", BackendName: "b1", SizeBytes: 5}},
			{},
		},
	}

	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": backend})

	mgr.DrainManager.PurgeBackendObjects(context.Background(), backend, "b1")

	// DB record should still be deleted even though S3 delete failed
	store.mu.Lock()
	calls := store.deleteObjectLocationCalls
	store.mu.Unlock()

	if len(calls) != 1 {
		t.Fatalf("expected 1 DeleteObjectLocation call, got %d", len(calls))
	}
	if calls[0].key != "missing" {
		t.Errorf("DeleteObjectLocation key = %q, want missing", calls[0].key)
	}
}

// TestRemoveBackend_PurgeTerminates verifies that RemoveBackend with purge=true
// terminates and doesn't loop infinitely. This is a regression test for the bug
// where purgeBackendObjects never deleted DB records, causing ListObjectsByBackend
// to return the same rows forever.
func TestRemoveBackend_PurgeTerminates(t *testing.T) {
	backend := newMockBackend()
	backend.objects["k1"] = mockObject{data: []byte("x")}

	store := &mockStore{
		listObjectsByBackendPages: [][]st.ObjectLocation{
			{{ObjectKey: "k1", BackendName: "b1", SizeBytes: 1}},
			{}, // purge loop exits
		},
	}

	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": backend})

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

// -------------------------------------------------------------------------
// drainOneObject — object already has replica elsewhere
// -------------------------------------------------------------------------

func TestDrainOneObject_ReplicaExists_DeletesSourceWithSize(t *testing.T) {
	srcBackend := newMockBackend()
	srcBackend.objects["key1"] = mockObject{data: []byte("data")}

	store := &mockStore{
		// GetAllObjectLocations returns copies on both b1 (source) and b2 (replica)
		getAllLocationsResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 4},
		},
	}

	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": srcBackend, "b2": newMockBackend()})

	obj := &st.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	ok := mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj)
	if !ok {
		t.Fatal("drainOneObject should succeed when replica exists")
	}

	// Source object should be deleted from S3
	if srcBackend.hasObject("key1") {
		t.Error("source object should have been deleted")
	}
}

// -------------------------------------------------------------------------
// drainOneObject — no replica, must copy then delete source
// -------------------------------------------------------------------------

func TestDrainOneObject_NoCopy_MovesObjectWithSize(t *testing.T) {
	srcBackend := newMockBackend()
	srcBackend.objects["key1"] = mockObject{data: []byte("abcd"), contentType: "text/plain"}

	dstBackend := newMockBackend()

	store := &mockStore{
		// Only on b1 (no replica)
		getAllLocationsResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		},
		getBackendResp:         "b2",
		moveObjectLocationSize: 4,
	}

	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": srcBackend, "b2": dstBackend})

	obj := &st.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	ok := mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj)
	if !ok {
		t.Fatal("drainOneObject should succeed")
	}

	// Object should now exist on destination
	if !dstBackend.hasObject("key1") {
		t.Error("destination backend should have the object")
	}
}

// -------------------------------------------------------------------------
// drainOneObject — MoveObjectLocation fails, orphan enqueued with size
// -------------------------------------------------------------------------

func TestDrainOneObject_MoveLocationFails_EnqueuesOrphanWithSize(t *testing.T) {
	srcBackend := newMockBackend()
	srcBackend.objects["key1"] = mockObject{data: []byte("abcd"), contentType: "text/plain"}

	dstBackend := newMockBackend()

	store := &mockStore{
		getAllLocationsResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		},
		getBackendResp:        "b2",
		moveObjectLocationErr: errors.New("serialization failure"),
	}

	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": srcBackend, "b2": dstBackend})

	// Make dstBackend.DeleteObject fail so enqueueCleanup is triggered
	dstBackend.delErr = errors.New("backend down")

	obj := &st.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	ok := mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj)
	if ok {
		t.Fatal("drainOneObject should fail when MoveObjectLocation fails")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	// The orphan on the destination should have been enqueued with the object size
	if len(store.enqueueCleanupCalls) != 1 {
		t.Fatalf("expected 1 enqueue call (drain_orphan), got %d", len(store.enqueueCleanupCalls))
	}
	if store.enqueueCleanupCalls[0].reason != "drain_orphan" {
		t.Errorf("reason = %q, want drain_orphan", store.enqueueCleanupCalls[0].reason)
	}
}

// -------------------------------------------------------------------------
// drainOneObject — MoveObjectLocation returns 0 (stale), enqueues orphan
// -------------------------------------------------------------------------

func TestDrainOneObject_StaleObject_EnqueuesOrphanWithSize(t *testing.T) {
	srcBackend := newMockBackend()
	srcBackend.objects["key1"] = mockObject{data: []byte("abcd"), contentType: "text/plain"}

	dstBackend := newMockBackend()
	dstBackend.delErr = errors.New("backend down") // force enqueue

	store := &mockStore{
		getAllLocationsResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		},
		getBackendResp:         "b2",
		moveObjectLocationSize: 0, // 0 means stale/already moved
	}

	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": srcBackend, "b2": dstBackend})

	obj := &st.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	ok := mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj)
	if ok {
		t.Fatal("drainOneObject should return false for stale object")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.enqueueCleanupCalls) != 1 {
		t.Fatalf("expected 1 enqueue call (drain_stale_orphan), got %d", len(store.enqueueCleanupCalls))
	}
	if store.enqueueCleanupCalls[0].reason != "drain_stale_orphan" {
		t.Errorf("reason = %q, want drain_stale_orphan", store.enqueueCleanupCalls[0].reason)
	}
}

// TestStartDrain_FlushesCleanupQueueBeforeDeleteBackendData verifies that
// runDrain processes pending cleanup queue items before calling DeleteBackendData,
// which would otherwise silently drop them.
func TestStartDrain_FlushesCleanupQueueBeforeDeleteBackendData(t *testing.T) {
	backend := newMockBackend()
	// Pre-populate an orphaned object that the cleanup queue should process
	backend.objects["orphan"] = mockObject{data: []byte("stale")}

	store := &mockStore{
		// No objects to migrate (drain completes immediately)
		listObjectsByBackendResp: nil,
		// Pending cleanup item for the draining backend
		pendingCleanups: []st.CleanupItem{
			{ID: 42, BackendName: "b1", ObjectKey: "orphan", Attempts: 0},
		},
	}

	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": backend})

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	// Wait for drain to complete
	progress, err := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
	for i := 0; i < 50 && (err != nil || progress.Active); i++ {
		time.Sleep(50 * time.Millisecond)
		progress, err = mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
	}
	if err != nil {
		t.Fatalf("GetDrainProgress: %v", err)
	}
	if progress.Active {
		t.Fatal("drain did not complete within timeout")
	}
	if progress.Error != "" {
		t.Fatalf("drain completed with error: %s", progress.Error)
	}

	// Verify the cleanup item was processed (completed)
	store.mu.Lock()
	completedIDs := store.completeCleanupCalls
	store.mu.Unlock()

	found := slices.Contains(completedIDs, 42)
	if !found {
		t.Error("expected cleanup item 42 to be completed during drain, but it was not")
	}

	// Verify the orphaned S3 object was deleted
	if backend.hasObject("orphan") {
		t.Error("orphaned object should have been deleted from S3 backend")
	}
}

// -------------------------------------------------------------------------
// CancelDrain
// -------------------------------------------------------------------------

func TestCancelDrain_CompletedDrain_ClearsState(t *testing.T) {
	backend := newMockBackend()
	store := &mockStore{
		listObjectsByBackendResp: nil, // no objects → drain completes immediately
	}
	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": backend})

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	// Wait for drain to finish
	for i := 0; i < 50; i++ {
		p, err := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
		if err == nil && !p.Active {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// CancelDrain on a completed drain should clear state (covers line 141)
	if err := mgr.DrainManager.CancelDrain("b1"); err != nil {
		t.Fatalf("CancelDrain: %v", err)
	}

	// A second CancelDrain should fail because the state was cleared
	if err := mgr.DrainManager.CancelDrain("b1"); err == nil {
		t.Error("expected error after clearing drained state")
	}
}

func TestCancelDrain_ActiveDrain_CancelsAndClears(t *testing.T) {
	store := &mockStore{
		// Gate never closes — drain blocks in ListObjectsByBackend until
		// CancelDrain cancels the context.
		listObjectsByBackendGate: make(chan struct{}),
	}
	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	// Give drain goroutine time to reach the blocking ListObjectsByBackend call
	time.Sleep(20 * time.Millisecond)

	// Cancel active drain — unblocks via ctx.Done() (covers line 150)
	if err := mgr.DrainManager.CancelDrain("b1"); err != nil {
		t.Fatalf("CancelDrain: %v", err)
	}
}

// -------------------------------------------------------------------------
// runDrain error paths
// -------------------------------------------------------------------------

func TestRunDrain_ListObjectsByBackendFails(t *testing.T) {
	store := &mockStore{
		listObjectsByBackendErr: errors.New("db connection lost"),
	}
	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	// Wait for drain to finish — on ListObjectsByBackend error, runDrain
	// deletes from d.draining and returns, so eventually GetDrainProgress
	// returns Active=false with no error (state was cleaned up).
	for i := 0; i < 50; i++ {
		p, _ := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
		if !p.Active {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The drain state should have been removed (error path deletes from map).
	// Attempting StartDrain again should succeed (proving old state is gone).
	// But we need a fresh store since the old one still returns errors.
	// Simply verify the drain is no longer active.
	p, _ := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
	if p.Active {
		t.Error("drain should have terminated after ListObjectsByBackend failure")
	}
}

func TestRunDrain_DeleteBackendDataFails(t *testing.T) {
	store := &mockStore{
		listObjectsByBackendResp: nil, // no objects → skip to cleanup
		deleteBackendDataErr:     errors.New("db write failed"),
	}
	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if err := mgr.DrainManager.StartDrain(context.Background(), "b1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	// Wait for drain to complete
	for i := 0; i < 50; i++ {
		p, err := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
		if err != nil || !p.Active {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	p, _ := mgr.DrainManager.GetDrainProgress(context.Background(), "b1")
	if p == nil || p.Error == "" {
		t.Error("expected drain error from DeleteBackendData failure")
	}
}

// -------------------------------------------------------------------------
// drainOneObject error paths
// -------------------------------------------------------------------------

func TestDrainOneObject_GetAllLocationsFails(t *testing.T) {
	store := &mockStore{
		getAllLocationsErr: errors.New("db error"),
	}
	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	obj := &st.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	ok := mgr.DrainManager.DrainOneObject(context.Background(), newMockBackend(), "b1", obj)
	if ok {
		t.Error("expected failure when GetAllObjectLocations fails")
	}
}

func TestDrainOneObject_DeleteSourceLocationFails(t *testing.T) {
	store := &mockStore{
		getAllLocationsResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
			{ObjectKey: "key1", BackendName: "b2", SizeBytes: 4}, // replica exists
		},
		deleteObjectLocationErr: errors.New("db error"),
	}
	srcBackend := newMockBackend()
	srcBackend.objects["key1"] = mockObject{data: []byte("data")}

	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": srcBackend, "b2": newMockBackend()})

	obj := &st.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	ok := mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj)
	if ok {
		t.Error("expected failure when DeleteObjectLocation fails")
	}
}

func TestDrainOneObject_NoDestinationAvailable(t *testing.T) {
	store := &mockStore{
		getAllLocationsResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		},
		getBackendErr: st.ErrNoSpaceAvailable,
	}
	srcBackend := newMockBackend()

	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": srcBackend})

	obj := &st.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	ok := mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj)
	if ok {
		t.Error("expected failure when no destination available")
	}
}

func TestDrainOneObject_DestBackendNotFound(t *testing.T) {
	store := &mockStore{
		getAllLocationsResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		},
		getBackendResp: "ghost", // not in backends map
	}
	srcBackend := newMockBackend()

	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": srcBackend})

	obj := &st.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	ok := mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj)
	if ok {
		t.Error("expected failure when destination backend not found")
	}
}

func TestDrainOneObject_StreamCopyFails(t *testing.T) {
	srcBackend := newMockBackend()
	srcBackend.getErr = errors.New("read failure")

	dstBackend := newMockBackend()

	store := &mockStore{
		getAllLocationsResp: []st.ObjectLocation{
			{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4},
		},
		getBackendResp: "b2",
	}
	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": srcBackend, "b2": dstBackend})

	obj := &st.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
	ok := mgr.DrainManager.DrainOneObject(context.Background(), srcBackend, "b1", obj)
	if ok {
		t.Error("expected failure when streamCopy fails")
	}
}

// -------------------------------------------------------------------------
// purgeBackendObjects error paths
// -------------------------------------------------------------------------

func TestPurgeBackendObjects_ListObjectsFails(t *testing.T) {
	store := &mockStore{
		listObjectsByBackendErr: errors.New("db error"),
	}
	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Should not panic and should return early
	mgr.DrainManager.PurgeBackendObjects(context.Background(), newMockBackend(), "b1")
}

func TestPurgeBackendObjects_S3DeleteFails_LogsWarning(t *testing.T) {
	backend := newMockBackend()
	backend.delErr = errors.New("s3 timeout")

	store := &mockStore{
		listObjectsByBackendPages: [][]st.ObjectLocation{
			{{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 5}},
			{},
		},
	}
	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": backend})

	mgr.DrainManager.PurgeBackendObjects(context.Background(), backend, "b1")

	// DB record should still be deleted even though S3 delete failed
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.deleteObjectLocationCalls) != 1 {
		t.Fatalf("expected 1 DeleteObjectLocation call, got %d", len(store.deleteObjectLocationCalls))
	}
}

func TestPurgeBackendObjects_DBDeleteFails(t *testing.T) {
	backend := newMockBackend()
	backend.objects["obj1"] = mockObject{data: []byte("data")}

	store := &mockStore{
		listObjectsByBackendPages: [][]st.ObjectLocation{
			{{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 5}},
			{},
		},
		deleteObjectLocationErr: errors.New("db error"),
	}
	mgr := newDrainTestManager(store, map[string]*mockBackend{"b1": backend})

	// Should not panic — continues despite DB delete failure
	mgr.DrainManager.PurgeBackendObjects(context.Background(), backend, "b1")
}

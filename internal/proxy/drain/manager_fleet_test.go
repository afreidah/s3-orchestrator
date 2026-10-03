// -------------------------------------------------------------------------------
// Drain Tests - Backend Purge and Remove Operations
//
// Author: Alex Freidah
//
// Unit tests for removing a backend. Validates that purge deletes DB records
// during iteration to avoid infinite loops, and that S3 objects are cleaned up.
// -------------------------------------------------------------------------------

package drain

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// purgedObjectSize is the per-object size every purge fixture stores, and so
// the bytes each ledger-row delete reports as freed.
const purgedObjectSize = 5

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// purgeCalls captures the ledger-row deletes a purge test asserts on.
type purgeCalls struct {
	mu              sync.Mutex
	deletedLocation []deleteLocationRecord
}

type deleteLocationRecord struct {
	key, backend string
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// pagedLister returns a DoAndReturn that hands out paginated
// ListObjectsByBackend results.
func pagedLister(pages [][]core.ObjectLocation) func(context.Context, string, int) ([]core.ObjectLocation, error) {
	idx := 0
	return func(context.Context, string, int) ([]core.ObjectLocation, error) {
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
func stubDeleteObjectLocation(c *purgeCalls, size int64, err error) func(context.Context, string, string) (int64, error) {
	return func(_ context.Context, key, backend string) (int64, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.deletedLocation = append(c.deletedLocation, deleteLocationRecord{key: key, backend: backend})
		return size, err
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

	c := &purgeCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{
				{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 5},
				{ObjectKey: "obj2", BackendName: "b1", SizeBytes: 5},
			},
			{},
		})).AnyTimes()
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

	c := &purgeCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{
				{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 5},
				{ObjectKey: "obj2", BackendName: "b1", SizeBytes: 5},
			},
			{},
		})).AnyTimes()
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

	c := &purgeCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{{ObjectKey: "missing", BackendName: "b1", SizeBytes: 5}},
			{},
		})).AnyTimes()
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
// process on the same rows until restart.
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
		})).AnyTimes()
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

	c := &purgeCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(pagedLister([][]core.ObjectLocation{
			{{ObjectKey: "obj1", BackendName: "b1", SizeBytes: 5}},
			{},
		})).AnyTimes()
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
		})).AnyTimes()
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(0), errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr, _ := newDrainFleet(t, store, map[string]backend.ObjectBackend{"b1": be})

	mgr.PurgeBackendObjects(context.Background(), be, "b1", nil)
}

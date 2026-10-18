// -------------------------------------------------------------------------------
// Drainer Tests
//
// Author: Alex Freidah
//
// The per-object moves run against a mock store so each branch's store calls
// can be pinned. The passes run against a real SQLite store and in-memory
// backends, so a drain is checked by where the bytes end up and the record it
// leaves behind.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/sqlite"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// drainedObject is the object every per-object test moves off b1.
func drainedObject() *core.ObjectLocation {
	return &core.ObjectLocation{ObjectKey: "key1", BackendName: "b1", SizeBytes: 4}
}

// onlyOnB1 and alsoOnB2 are what the page lookup reports holding the drained
// object's copies: the draining backend alone, or it and another.
var (
	onlyOnB1 = []string{"b1"}
	alsoOnB2 = []string{"b1", "b2"}
)

// drainedCleanupID is the id of the cleanup row the drained copy's removal
// queues.
const drainedCleanupID = 41

// removesDrainedCopy has the conditional removal drop the drained copy and
// queue its bytes under the drain's source-delete reason.
func removesDrainedCopy(store *storetest.MockMetadataStore) {
	store.EXPECT().DeleteObjectLocation(gomock.Any(), drainedObject(), 1, writepath.DrainMove.Reasons.SourceDelete).
		Return([]core.CleanupItem{{
			ID: drainedCleanupID, BackendName: "b1", ObjectKey: "key1", StorageKey: "key1",
			Reason: writepath.DrainMove.Reasons.SourceDelete, SizeBytes: 4,
		}}, nil).AnyTimes()
}

// captureCompleted records the id of every cleanup row retired.
func captureCompleted(store *storetest.MockMetadataStore) *[]int64 {
	var (
		mu  sync.Mutex
		ids []int64
	)
	store.EXPECT().CompleteCleanupItem(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, id int64) error {
			mu.Lock()
			defer mu.Unlock()
			ids = append(ids, id)
			return nil
		}).AnyTimes()
	return &ids
}

// newDrainStore returns a SQLite store that knows the named backends.
func newDrainStore(t *testing.T, backends ...string) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	s, err := sqlite.NewStore(ctx, &config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"}, nil, "test-instance")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(s.Close)
	cfgs := make([]config.BackendConfig, len(backends))
	for i, name := range backends {
		cfgs[i] = config.BackendConfig{Name: name, QuotaBytes: 1 << 30}
	}
	if err := s.SyncQuotaLimits(ctx, cfgs); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	return s
}

// putOn stores an object's bytes on be and records its only copy there.
func putOn(t *testing.T, s *sqlite.Store, be *backendtest.InMemory, backendName, key string) {
	t.Helper()
	data := []byte("data-" + key)
	be.Objects[key] = backendtest.Object{Data: data}
	if _, _, err := s.RecordObject(context.Background(), &core.RecordObjectRequest{
		Key: key, Copies: []core.ObjectCopy{{Backend: backendName}}, Size: int64(len(data)),
	}); err != nil {
		t.Fatalf("RecordObject(%s): %v", key, err)
	}
}

// drainRecord returns the backend's drain record, or a zero record when it has
// none.
func drainRecord(t *testing.T, s *sqlite.Store, backend string) core.BackendStateRecord {
	t.Helper()
	drains, err := s.ListBackendStates(context.Background())
	if err != nil {
		t.Fatalf("ListBackendStates: %v", err)
	}
	for _, d := range drains {
		if d.BackendName == backend {
			return d
		}
	}
	return core.BackendStateRecord{}
}

// startDrain records a drain of the backend.
func startDrain(t *testing.T, s *sqlite.Store, backend string) {
	t.Helper()
	if started, err := s.StartDrain(context.Background(), backend); err != nil || !started {
		t.Fatalf("StartDrain(%s) = %v, %v", backend, started, err)
	}
}

// -------------------------------------------------------------------------
// PER-OBJECT MOVES
// -------------------------------------------------------------------------

// TestDrainOne_ReplicaElsewhere_DropsTheDrainedCopy verifies an object another
// backend also holds loses only its copy on the draining backend, and that the
// cleanup row its removal queued is settled.
func TestDrainOne_ReplicaElsewhere_DropsTheDrainedCopy(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	src.Objects["key1"] = backendtest.Object{Data: []byte("data")}

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	removesDrainedCopy(store)
	completed := captureCompleted(store)
	storetest.Permissive(store)

	d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": backendtest.NewInMemory()}, nil)
	if !d.drainOne(context.Background(), src, "b1", drainedObject(), alsoOnB2) {
		t.Fatal("drainOne failed with a copy on another backend")
	}
	if src.Has("key1") {
		t.Error("the draining backend still holds its copy")
	}
	if len(*completed) != 1 || (*completed)[0] != drainedCleanupID {
		t.Errorf("retired cleanup rows %v, want the drained copy's row", *completed)
	}
}

// TestDrainOne_OtherCopyGone_MovesInstead verifies that when the copy the page
// lookup saw elsewhere is gone by the time the drop runs, the conditional
// removal keeps the drained copy and the object is moved off instead of lost.
func TestDrainOne_OtherCopyGone_MovesInstead(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	src.Objects["key1"] = backendtest.Object{Data: []byte("abcd"), ContentType: "text/plain"}
	dst := backendtest.NewInMemory()

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().DeleteObjectLocation(gomock.Any(), drainedObject(), 1, gomock.Any()).Return(nil, core.ErrCopyNotExcess)
	moves := 0
	store.EXPECT().PlaceCopy(gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, *core.Placement) (int64, []core.CleanupItem, error) {
			moves++
			return 4, nil, nil
		}).AnyTimes()
	storetest.Permissive(store)

	d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": dst}, &fleetOpts{Order: []string{"b1", "b2"}})
	if !d.drainOne(context.Background(), src, "b1", drainedObject(), alsoOnB2) {
		t.Fatal("drainOne failed to move the object once its other copy was gone")
	}
	if moves != 1 {
		t.Errorf("moved %d times, want the object moved off once", moves)
	}
}

// TestDrainOne_KeepsTheOnlyUsableKey verifies a drained copy that holds the only
// usable encryption key is kept and the item fails, rather than the drop
// leaving the object's other copies undecryptable.
func TestDrainOne_KeepsTheOnlyUsableKey(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	src.Objects["key1"] = backendtest.Object{Data: []byte("data")}

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().DeleteObjectLocation(gomock.Any(), drainedObject(), 1, gomock.Any()).Return(nil, core.ErrCopyHoldsOnlyDEK)
	storetest.Permissive(store)

	d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": backendtest.NewInMemory()}, nil)
	if d.drainOne(context.Background(), src, "b1", drainedObject(), alsoOnB2) {
		t.Fatal("drainOne dropped the copy holding the only usable key")
	}
	if !src.Has("key1") {
		t.Error("the copy holding the only usable key was deleted")
	}
}

// TestDrainOne_OnlyCopy_MovesTheObject verifies an object only the draining
// backend holds is streamed to a path of its own on another backend, and that
// the drained copy is placed there and removed from the draining backend.
func TestDrainOne_OnlyCopy_MovesTheObject(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	src.Objects["key1"] = backendtest.Object{Data: []byte("abcd"), ContentType: "text/plain"}
	dst := backendtest.NewInMemory()

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	var placed core.Placement
	store.EXPECT().PlaceCopy(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.Placement) (int64, []core.CleanupItem, error) {
			placed = *p
			return 4, nil, nil
		}).AnyTimes()
	storetest.Permissive(store)

	d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": dst}, &fleetOpts{Order: []string{"b1", "b2"}})
	obj := drainedObject()
	if !d.drainOne(context.Background(), src, "b1", obj, onlyOnB1) {
		t.Fatal("drainOne failed to move the only copy")
	}
	if placed.Source != obj || placed.Intent == nil || placed.Intent.BackendName != "b2" || !placed.RemoveSource {
		t.Fatalf("placement = %+v, want the drained copy moved to b2", placed)
	}
	if placed.Reasons != writepath.DrainMove.Reasons {
		t.Errorf("placement reasons = %+v, want the drain's", placed.Reasons)
	}
	path := placed.Intent.StorageKey
	if path == "" || path == "key1" || !dst.Has(path) {
		t.Errorf("copy placed at path %q; the destination must hold the bytes at a fresh path", path)
	}
}

// TestDrainOne_RefusedMove_SettlesTheCopyItLeft verifies a move whose placement
// is declined because the object changed or the target refused it settles the
// cleanup row the refusal queued, so the destination copy is deleted.
func TestDrainOne_RefusedMove_SettlesTheCopyItLeft(t *testing.T) {
	t.Parallel()
	for _, refusal := range []error{core.ErrCopyChanged, core.ErrTargetRefused} {
		t.Run(refusal.Error(), func(t *testing.T) {
			t.Parallel()
			src := backendtest.NewInMemory()
			src.Objects["key1"] = backendtest.Object{Data: []byte("abcd"), ContentType: "text/plain"}
			dst := backendtest.NewInMemory()

			store := storetest.NewMockMetadataStore(gomock.NewController(t))
			var path string
			store.EXPECT().PlaceCopy(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, p *core.Placement) (int64, []core.CleanupItem, error) {
					path = p.Intent.StorageKey
					return 0, []core.CleanupItem{{
						ID: drainedCleanupID, BackendName: "b2", ObjectKey: "key1", StorageKey: path,
						Reason: p.Reasons.Orphan, SizeBytes: 4,
					}}, refusal
				})
			completed := captureCompleted(store)
			storetest.Permissive(store)

			d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": dst}, &fleetOpts{Order: []string{"b1", "b2"}})
			if d.drainOne(context.Background(), src, "b1", drainedObject(), onlyOnB1) {
				t.Fatal("drainOne reported success")
			}
			if dst.Has(path) {
				t.Error("the refused copy is still on the destination")
			}
			if len(*completed) != 1 || (*completed)[0] != drainedCleanupID {
				t.Errorf("retired cleanup rows %v, want the refused copy's row", *completed)
			}
		})
	}
}

// TestDrainOne_PlacementError_LeavesTheCopyForTheReaper verifies a placement
// that fails outright queues nothing, so the destination copy stays under its
// intent for the pending reaper to resolve.
func TestDrainOne_PlacementError_LeavesTheCopyForTheReaper(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	src.Objects["key1"] = backendtest.Object{Data: []byte("abcd"), ContentType: "text/plain"}
	dst := backendtest.NewInMemory()

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	var path string
	store.EXPECT().PlaceCopy(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.Placement) (int64, []core.CleanupItem, error) {
			path = p.Intent.StorageKey
			return 0, nil, errors.New("serialization failure")
		})
	completed := captureCompleted(store)
	storetest.Permissive(store)

	d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": dst}, &fleetOpts{Order: []string{"b1", "b2"}})
	if d.drainOne(context.Background(), src, "b1", drainedObject(), onlyOnB1) {
		t.Fatal("drainOne reported success")
	}
	if !dst.Has(path) {
		t.Error("the unplaced copy was deleted; the reaper resolves it through its intent")
	}
	if len(*completed) != 0 {
		t.Errorf("retired cleanup rows %v, want none", *completed)
	}
}

// TestDrainOne_Failures verifies each way a single move can fail reports
// failure rather than success.
func TestDrainOne_Failures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		copies []string
		setup  func(store *storetest.MockMetadataStore, src *backendtest.InMemory) map[string]backend.ObjectBackend
	}{
		{"dropping the drained row fails", alsoOnB2, func(store *storetest.MockMetadataStore, src *backendtest.InMemory) map[string]backend.ObjectBackend {
			store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
				Return(nil, errors.New("db error")).AnyTimes()
			return map[string]backend.ObjectBackend{"b1": src, "b2": backendtest.NewInMemory()}
		}},
		{"no destination", onlyOnB1, func(_ *storetest.MockMetadataStore, src *backendtest.InMemory) map[string]backend.ObjectBackend {
			return map[string]backend.ObjectBackend{"b1": src}
		}},
		{"the only other backend refuses reads", onlyOnB1, func(_ *storetest.MockMetadataStore, src *backendtest.InMemory) map[string]backend.ObjectBackend {
			return map[string]backend.ObjectBackend{"b1": src, "b2": backendtest.RefusingReads("b2")}
		}},
		{"source read fails", onlyOnB1, func(_ *storetest.MockMetadataStore, src *backendtest.InMemory) map[string]backend.ObjectBackend {
			src.GetErr = errors.New("read failure")
			return map[string]backend.ObjectBackend{"b1": src, "b2": backendtest.NewInMemory()}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := backendtest.NewInMemory()
			src.Objects["key1"] = backendtest.Object{Data: []byte("data")}
			store := storetest.NewMockMetadataStore(gomock.NewController(t))
			backends := tc.setup(store, src)
			storetest.Permissive(store)

			d := newDrainerFor(t, store, backends, nil)
			if d.drainOne(context.Background(), src, "b1", drainedObject(), tc.copies) {
				t.Error("drainOne reported success")
			}
		})
	}
}

// TestMovePage_CopyLookupFailureEndsThePage verifies a page whose copy lookup
// fails returns the error for the next tick to retry, without moving anything.
func TestMovePage_CopyLookupFailureEndsThePage(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetObjectBackendsForKeys(gomock.Any(), []string{"key1"}).Return(nil, errors.New("db error"))
	storetest.Permissive(store)

	d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": src}, nil)
	sum, err := d.movePage(context.Background(), src, "b1", []core.ObjectLocation{*drainedObject()}, nil)
	if err == nil {
		t.Fatal("movePage succeeded with a failed copy lookup")
	}
	if sum.Attempted != 0 {
		t.Errorf("attempted %d moves after the lookup failed", sum.Attempted)
	}
}

// TestMoveOff_UsesTheDrainReasons verifies a move off the draining backend
// goes to the emptiest other backend under the drain's cleanup reasons.
func TestMoveOff_UsesTheDrainReasons(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	src, dest := backendtest.NewMockObjectBackend(ctrl), backendtest.NewMockObjectBackend(ctrl)
	store := newPermissiveStore(t)
	rt, _ := newFleet(t, store, map[string]backend.ObjectBackend{"src": src, "dest": dest}, &fleetOpts{
		Order: []string{"src", "dest"},
		QuotaBaselines: map[string]core.BackendQuotaUsage{
			"src":  {BackendName: "src", BytesLimit: 100, BytesUsed: 90},
			"dest": {BackendName: "dest", BytesLimit: 100},
		},
	})
	placement := NewMockPlacement(ctrl)
	var got *writepath.MoveRequest
	placement.EXPECT().MoveObject(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, req *writepath.MoveRequest) (int64, error) {
			got = req
			return req.Source.SizeBytes, nil
		})

	d := NewDrainer(DrainerDeps{Ops: rt, Placement: placement, Store: store, AbortUploads: func(context.Context, string) {}})
	obj := &core.ObjectLocation{ObjectKey: "k", SizeBytes: 50, BackendName: "src"}
	if !d.moveOff(context.Background(), src, "src", obj) {
		t.Fatal("moveOff reported failure")
	}
	if got == nil || got.Profile != writepath.DrainMove || got.Source != obj || got.DestName != "dest" {
		t.Errorf("move request = %+v, want src -> dest under the DrainMove profile", got)
	}
}

// -------------------------------------------------------------------------
// PASSES
// -------------------------------------------------------------------------

// TestDrain_MovesEverythingOffAndCompletes verifies a pass moves every object
// off the draining backend, marks the drain drained, aborts the backend's
// uploads, and hands the records it read to OnRecords.
func TestDrain_MovesEverythingOffAndCompletes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newDrainStore(t, "b1", "b2")
	b1, b2 := backendtest.NewInMemory(), backendtest.NewInMemory()
	for _, key := range []string{"bkt/a", "bkt/b", "bkt/c"} {
		putOn(t, store, b1, "b1", key)
	}
	startDrain(t, store, "b1")

	rt, coord := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})
	var aborted []string
	var seen []core.BackendStateRecord
	d := NewDrainer(DrainerDeps{
		Ops: rt, Placement: coord, Store: store,
		AbortUploads: func(_ context.Context, name string) { aborted = append(aborted, name) },
		OnRecords:    func(drains []core.BackendStateRecord) { seen = drains },
	})

	sum, err := d.Drain(ctx, nil)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if sum.Succeeded != 3 || sum.Completed != 1 {
		t.Errorf("summary = %+v, want 3 moved and 1 drain completed", sum)
	}
	for _, key := range []string{"bkt/a", "bkt/b", "bkt/c"} {
		if b1.HasCopyOf(key) || !b2.HasCopyOf(key) {
			t.Errorf("%s: on b1 = %v, on b2 = %v; want moved to b2", key, b1.HasCopyOf(key), b2.HasCopyOf(key))
		}
	}
	if rec := drainRecord(t, store, "b1"); rec.Flags != core.FlagDrained || rec.ObjectsMoved != 3 {
		t.Errorf("record = %+v, want drained with 3 objects moved", rec)
	}
	if len(aborted) != 1 || aborted[0] != "b1" {
		t.Errorf("aborted uploads on %v, want b1", aborted)
	}
	if len(seen) != 1 || seen[0].BackendName != "b1" {
		t.Errorf("OnRecords got %+v, want the b1 record", seen)
	}
}

// TestDrain_WaitsForAnUploadAdmittedBeforeTheDrain verifies the drain stays in
// progress while a write admitted before it is still uploading, and completes
// once that write resolves.
func TestDrain_WaitsForAnUploadAdmittedBeforeTheDrain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newDrainStore(t, "b1", "b2")
	if ok, err := store.InsertPendingIfFits(ctx, &core.PendingObject{IntentID: "in-flight", ObjectKey: "bkt/k", BackendName: "b1", SizeBytes: 1}, core.AcceptForWrite); err != nil || !ok {
		t.Fatalf("InsertPendingIfFits = %v, %v", ok, err)
	}
	startDrain(t, store, "b1")
	d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory(), "b2": backendtest.NewInMemory()}, nil)

	if sum, err := d.Drain(ctx, nil); err != nil || sum.Completed != 0 {
		t.Fatalf("Drain = %+v, %v; want the drain left in progress", sum, err)
	}
	if rec := drainRecord(t, store, "b1"); rec.Flags != core.FlagDraining {
		t.Fatalf("record = %+v, want still draining", rec)
	}

	if err := store.DeletePending(ctx, "in-flight"); err != nil {
		t.Fatalf("DeletePending: %v", err)
	}
	if sum, err := d.Drain(ctx, nil); err != nil || sum.Completed != 1 {
		t.Fatalf("Drain = %+v, %v; want the drain completed", sum, err)
	}
}

// TestDrain_PageThatMovesNothingEndsThePass verifies a page whose objects all
// fail to move ends the pass with the drain still in progress, rather than
// listing the same page forever.
func TestDrain_PageThatMovesNothingEndsThePass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newDrainStore(t, "b1")
	b1 := backendtest.NewInMemory()
	putOn(t, store, b1, "b1", "bkt/stuck")
	startDrain(t, store, "b1")

	// b1 is the only backend, so there is nowhere to move the object to.
	d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": b1}, nil)
	sum, err := d.Drain(ctx, nil)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if sum.Failed != 1 || sum.Completed != 0 {
		t.Errorf("summary = %+v, want the one object failed and nothing completed", sum)
	}
	if rec := drainRecord(t, store, "b1"); rec.Flags != core.FlagDraining {
		t.Errorf("record = %+v, want still draining for the next tick", rec)
	}
}

// TestDrain_IgnoresBackendsNotDraining verifies a pass moves nothing for a
// drain that was cleared, or that already finished or failed.
func TestDrain_IgnoresBackendsNotDraining(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newDrainStore(t, "b1", "b2")
	b1 := backendtest.NewInMemory()
	putOn(t, store, b1, "b1", "bkt/k")
	startDrain(t, store, "b1")
	if err := store.MarkDrainFailed(ctx, "b1", "stopped"); err != nil {
		t.Fatalf("MarkDrainFailed: %v", err)
	}

	d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": backendtest.NewInMemory()}, nil)
	if sum, err := d.Drain(ctx, nil); err != nil || sum.Attempted != 0 {
		t.Errorf("Drain = %+v, %v; want nothing attempted", sum, err)
	}
	if !b1.Has("bkt/k") {
		t.Error("a failed drain still moved the object")
	}
}

// TestDrain_UnconfiguredBackendFailsTheDrain verifies a drain record for a
// backend the fleet no longer has is marked failed rather than retried forever.
func TestDrain_UnconfiguredBackendFailsTheDrain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newDrainStore(t, "b1", "gone")
	startDrain(t, store, "gone")

	d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)
	if _, err := d.Drain(ctx, nil); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if rec := drainRecord(t, store, "gone"); rec.Flags != core.FlagDrainFailed || rec.LastError == "" {
		t.Errorf("record = %+v, want failed with a reason", rec)
	}
}

// TestDrain_ListFailures verifies a store error listing the backend's objects
// fails the drain, while the database being unavailable is left for the next
// tick without touching the record.
func TestDrain_ListFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		listErr    error
		wantFailed bool
		wantErr    bool
	}{
		{"store error fails the drain", errors.New("bad query"), true, false},
		{"database unavailable retries", core.ErrDBUnavailable, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := storetest.NewMockMetadataStore(gomock.NewController(t))
			store.EXPECT().ListBackendStates(gomock.Any()).
				Return([]core.BackendStateRecord{{BackendName: "b1", Flags: core.FlagDraining}}, nil).AnyTimes()
			store.EXPECT().ListObjectsByBackend(gomock.Any(), "b1", gomock.Any(), gomock.Any()).Return(nil, tc.listErr).AnyTimes()
			failed := false
			store.EXPECT().MarkDrainFailed(gomock.Any(), "b1", gomock.Any()).
				DoAndReturn(func(context.Context, string, string) error { failed = true; return nil }).AnyTimes()
			storetest.Permissive(store)

			d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)
			_, err := d.Drain(context.Background(), nil)
			if (err != nil) != tc.wantErr {
				t.Errorf("Drain err = %v, want error %v", err, tc.wantErr)
			}
			if failed != tc.wantFailed {
				t.Errorf("drain marked failed = %v, want %v", failed, tc.wantFailed)
			}
		})
	}
}

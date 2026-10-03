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

// onlyOnB1 answers GetAllObjectLocations with a single copy on b1.
func onlyOnB1(store *storetest.MockMetadataStore) {
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{*drainedObject()}, nil).AnyTimes()
}

// captureEnqueue records the reason of every cleanup row enqueued.
func captureEnqueue(store *storetest.MockMetadataStore) *[]string {
	var (
		mu      sync.Mutex
		reasons []string
	)
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _, reason string, _ int64) error {
			mu.Lock()
			defer mu.Unlock()
			reasons = append(reasons, reason)
			return nil
		}).AnyTimes()
	return &reasons
}

// newDrainStore returns a SQLite store that knows the named backends.
func newDrainStore(t *testing.T, backends ...string) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	s, err := sqlite.NewStore(ctx, &config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"}, nil)
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
func drainRecord(t *testing.T, s *sqlite.Store, backend string) core.BackendDrain {
	t.Helper()
	drains, err := s.ListDrains(context.Background())
	if err != nil {
		t.Fatalf("ListDrains: %v", err)
	}
	for _, d := range drains {
		if d.BackendName == backend {
			return d
		}
	}
	return core.BackendDrain{}
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
// backend also holds loses only its copy on the draining backend.
func TestDrainOne_ReplicaElsewhere_DropsTheDrainedCopy(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	src.Objects["key1"] = backendtest.Object{Data: []byte("data")}

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{*drainedObject(), {ObjectKey: "key1", BackendName: "b2", SizeBytes: 4}}, nil).AnyTimes()
	storetest.Permissive(store)

	d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": backendtest.NewInMemory()}, nil)
	if !d.drainOne(context.Background(), src, "b1", drainedObject()) {
		t.Fatal("drainOne failed with a copy on another backend")
	}
	if src.Has("key1") {
		t.Error("the draining backend still holds its copy")
	}
}

// TestDrainOne_OnlyCopy_MovesTheObject verifies an object only the draining
// backend holds is streamed to another backend.
func TestDrainOne_OnlyCopy_MovesTheObject(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	src.Objects["key1"] = backendtest.Object{Data: []byte("abcd"), ContentType: "text/plain"}
	dst := backendtest.NewInMemory()

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	onlyOnB1(store)
	store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(4), nil).AnyTimes()
	storetest.Permissive(store)

	d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": dst}, &fleetOpts{Order: []string{"b1", "b2"}})
	if !d.drainOne(context.Background(), src, "b1", drainedObject()) {
		t.Fatal("drainOne failed to move the only copy")
	}
	if !dst.Has("key1") {
		t.Error("the destination does not hold the object")
	}
}

// TestDrainOne_MoveFailures_EnqueueTheCopyTheyLeft verifies a move whose row
// change fails, or finds the object already gone, queues the destination copy
// for cleanup under the drain's own reasons.
func TestDrainOne_MoveFailures_EnqueueTheCopyTheyLeft(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		moved  int64
		err    error
		reason string
	}{
		{"row change fails", 0, errors.New("serialization failure"), "drain_orphan"},
		{"object already gone", 0, nil, "drain_stale_orphan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := backendtest.NewInMemory()
			src.Objects["key1"] = backendtest.Object{Data: []byte("abcd"), ContentType: "text/plain"}
			dst := backendtest.NewInMemory()
			dst.DeleteErr = errors.New("backend down")

			store := storetest.NewMockMetadataStore(gomock.NewController(t))
			onlyOnB1(store)
			store.EXPECT().MoveObjectLocation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
				Return(tc.moved, tc.err).AnyTimes()
			reasons := captureEnqueue(store)
			storetest.Permissive(store)

			d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": dst}, &fleetOpts{Order: []string{"b1", "b2"}})
			if d.drainOne(context.Background(), src, "b1", drainedObject()) {
				t.Fatal("drainOne reported success")
			}
			if len(*reasons) != 1 || (*reasons)[0] != tc.reason {
				t.Errorf("enqueued %v, want one %s row", *reasons, tc.reason)
			}
		})
	}
}

// TestDrainOne_Failures verifies each way a single move can fail reports
// failure rather than success.
func TestDrainOne_Failures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(store *storetest.MockMetadataStore, src *backendtest.InMemory) map[string]backend.ObjectBackend
	}{
		{"location lookup fails", func(store *storetest.MockMetadataStore, src *backendtest.InMemory) map[string]backend.ObjectBackend {
			store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).Return(nil, errors.New("db error")).AnyTimes()
			return map[string]backend.ObjectBackend{"b1": src}
		}},
		{"dropping the drained row fails", func(store *storetest.MockMetadataStore, src *backendtest.InMemory) map[string]backend.ObjectBackend {
			store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
				Return([]core.ObjectLocation{*drainedObject(), {ObjectKey: "key1", BackendName: "b2", SizeBytes: 4}}, nil).AnyTimes()
			store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
				Return(int64(0), errors.New("db error")).AnyTimes()
			return map[string]backend.ObjectBackend{"b1": src, "b2": backendtest.NewInMemory()}
		}},
		{"no destination", func(store *storetest.MockMetadataStore, src *backendtest.InMemory) map[string]backend.ObjectBackend {
			onlyOnB1(store)
			return map[string]backend.ObjectBackend{"b1": src}
		}},
		{"source read fails", func(store *storetest.MockMetadataStore, src *backendtest.InMemory) map[string]backend.ObjectBackend {
			onlyOnB1(store)
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
			if d.drainOne(context.Background(), src, "b1", drainedObject()) {
				t.Error("drainOne reported success")
			}
		})
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
			return req.SizeBytes, nil
		})

	d := NewDrainer(DrainerDeps{Ops: rt, Placement: placement, Store: store, AbortUploads: func(context.Context, string) {}})
	if !d.moveOff(context.Background(), src, "src", &core.ObjectLocation{ObjectKey: "k", SizeBytes: 50, BackendName: "src"}) {
		t.Fatal("moveOff reported failure")
	}
	if got == nil || got.Reasons != writepath.DrainMoveReasons || got.SrcName != "src" || got.DestName != "dest" {
		t.Errorf("move request = %+v, want src -> dest under DrainMoveReasons", got)
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
	var seen []core.BackendDrain
	d := NewDrainer(DrainerDeps{
		Ops: rt, Placement: coord, Store: store,
		AbortUploads: func(_ context.Context, name string) { aborted = append(aborted, name) },
		OnRecords:    func(drains []core.BackendDrain) { seen = drains },
	})

	sum, err := d.Drain(ctx, nil)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if sum.Succeeded != 3 || sum.Completed != 1 {
		t.Errorf("summary = %+v, want 3 moved and 1 drain completed", sum)
	}
	for _, key := range []string{"bkt/a", "bkt/b", "bkt/c"} {
		if b1.Has(key) || !b2.Has(key) {
			t.Errorf("%s: on b1 = %v, on b2 = %v; want moved to b2", key, b1.Has(key), b2.Has(key))
		}
	}
	if rec := drainRecord(t, store, "b1"); rec.State != core.DrainStateDrained || rec.ObjectsMoved != 3 {
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
	if ok, err := store.InsertPendingIfFits(ctx, &core.PendingObject{IntentID: "in-flight", ObjectKey: "bkt/k", BackendName: "b1", SizeBytes: 1}); err != nil || !ok {
		t.Fatalf("InsertPendingIfFits = %v, %v", ok, err)
	}
	startDrain(t, store, "b1")
	d := newDrainerFor(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory(), "b2": backendtest.NewInMemory()}, nil)

	if sum, err := d.Drain(ctx, nil); err != nil || sum.Completed != 0 {
		t.Fatalf("Drain = %+v, %v; want the drain left in progress", sum, err)
	}
	if rec := drainRecord(t, store, "b1"); rec.State != core.DrainStateDraining {
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
	if rec := drainRecord(t, store, "b1"); rec.State != core.DrainStateDraining {
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
	if rec := drainRecord(t, store, "gone"); rec.State != core.DrainStateFailed || rec.LastError == "" {
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
			store.EXPECT().ListDrains(gomock.Any()).
				Return([]core.BackendDrain{{BackendName: "b1", State: core.DrainStateDraining}}, nil).AnyTimes()
			store.EXPECT().ListObjectsByBackend(gomock.Any(), "b1", gomock.Any()).Return(nil, tc.listErr).AnyTimes()
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

// -------------------------------------------------------------------------------
// Reconciler Tests
//
// Author: Alex Freidah
//
// Tests for the background orphan reconciler: imports untracked objects,
// skips already-tracked objects, handles backend errors gracefully.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// TestMakeReconcileDeleter_SweepsCleanupQueue asserts the composed
// deleter calls DeleteObjectLocation followed by SweepStaleCleanupQueueRows.
func TestMakeReconcileDeleter_SweepsCleanupQueue(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	type sweepCall struct{ key, backend string }
	var sweeps []sweepCall
	store.EXPECT().SweepStaleCleanupQueueRows(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, key, backend string) (int64, error) {
			sweeps = append(sweeps, sweepCall{key: key, backend: backend})
			return 0, nil
		}).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	deleter := mgr.makeReconcileDeleter()
	if err := deleter(context.Background(), "bucket/k1", "b1"); err != nil {
		t.Fatalf("deleter returned error: %v", err)
	}

	if len(sweeps) != 1 {
		t.Fatalf("SweepStaleCleanupQueueRows calls = %d, want 1", len(sweeps))
	}
	if sweeps[0].key != "bucket/k1" || sweeps[0].backend != "b1" {
		t.Errorf("sweep called with %+v, want {key:bucket/k1 backend:b1}", sweeps[0])
	}
}

// TestMakeReconcileDeleter_SweepFailureNotPropagated asserts a sweep
// error is logged and swallowed.
func TestMakeReconcileDeleter_SweepFailureNotPropagated(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().SweepStaleCleanupQueueRows(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(int64(0), errors.New("db blip")).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	deleter := mgr.makeReconcileDeleter()
	if err := deleter(context.Background(), "bucket/k1", "b1"); err != nil {
		t.Errorf("deleter must not propagate sweep failure, got %v", err)
	}
}

// TestMakeReconcileDeleter_DeleteFailurePropagates asserts a delete
// failure stops before the sweep is attempted.
func TestMakeReconcileDeleter_DeleteFailurePropagates(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(errors.New("db blip")).
		AnyTimes()
	var sweeps int
	store.EXPECT().SweepStaleCleanupQueueRows(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string) (int64, error) {
			sweeps++
			return 0, nil
		}).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	deleter := mgr.makeReconcileDeleter()
	if err := deleter(context.Background(), "bucket/k1", "b1"); err == nil {
		t.Fatal("expected delete failure to propagate")
	}
	if sweeps != 0 {
		t.Errorf("sweep should be skipped when delete fails, got %d calls", sweeps)
	}
}

// TestReconciler_ImportsUntrackedObjects exercises the reconciler entry
// point against a mock backend that doesn't support listing.
func TestReconciler_ImportsUntrackedObjects(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newTestManager(t, store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
	})

	reconciler := worker.NewReconciler(mgr, []string{"unified"})
	reconciler.Run(context.Background())
}

// TestReconciler_NoBuckets verifies the no-buckets early return.
func TestReconciler_NoBuckets(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	reconciler := worker.NewReconciler(mgr, []string{})
	reconciler.Run(context.Background())
}

// TestReconciler_CancelledContext verifies an already-cancelled ctx
// returns quickly.
func TestReconciler_CancelledContext(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	reconciler := worker.NewReconciler(mgr, []string{"unified"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reconciler.Run(ctx)
}

// TestReconciler_RunDoesNotPanicOnBackendError exercises the
// import-error path.
func TestReconciler_RunDoesNotPanicOnBackendError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ImportObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(false, errors.New("db error")).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	reconciler := worker.NewReconciler(mgr, []string{"unified"})
	reconciler.Run(context.Background())
}

// TestReconcileBackend_UnknownBackend verifies the unknown-backend
// error path.
func TestReconcileBackend_UnknownBackend(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := mgr.ReconcileBackend(context.Background(), "nonexistent", "unified", []string{"unified"}); err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

// TestReconcileBackend_ListingNotSupported verifies the listing-not-
// supported error path.
func TestReconcileBackend_ListingNotSupported(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := mgr.ReconcileBackend(context.Background(), "b1", "unified", []string{"unified"}); err == nil {
		t.Fatal("expected error for backend that doesn't support listing")
	}
}

// TestReconcile_ViaReconciler exercises the reconciler-level entry
// against a backend that fails listing.
func TestReconcile_ViaReconciler(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	reconciler := worker.NewReconciler(mgr, []string{"unified"})
	result, err := reconciler.Reconcile(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.BackendsScanned != 0 {
		t.Errorf("backends_scanned = %d, want 0 (all failed)", result.BackendsScanned)
	}
}

// TestReconcile_SingleBackendViaReconciler exercises the single-backend
// path.
func TestReconcile_SingleBackendViaReconciler(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": newMockBackend()})

	reconciler := worker.NewReconciler(mgr, []string{"unified"})
	result, err := reconciler.Reconcile(context.Background(), "b1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.BackendsScanned != 0 {
		t.Errorf("backends_scanned = %d, want 0 (listing not supported)", result.BackendsScanned)
	}
}
// -------------------------------------------------------------------------
// ReconcileBackend  -  manager-level happy and error paths
// -------------------------------------------------------------------------

// listingMockBackend satisfies both backend.ObjectBackend (via the embedded
// mockBackend) and the proxy.ObjectLister contract used by reconcile.
// Tests can wire it into newTestManager and then drive ReconcileBackend
// end-to-end against a deterministic key list.
type listingMockBackend struct {
	*mockBackend
	pages [][]backend.ListedObject
	err   error
}

// ListObjects feeds the configured pages into the callback, mirroring the
// real S3 backend's signature.
func (l *listingMockBackend) ListObjects(_ context.Context, _ string, fn func([]backend.ListedObject) error) error {
	for _, p := range l.pages {
		if err := fn(p); err != nil {
			return err
		}
	}
	return l.err
}

// TestReconcileBackend_HappyPathImportsAndDeletes drives a fully wired
// BackendManager through ReconcileBackend with a backend that lists three
// keys (a, b, c). The DB cursor returns ("b", "x"), so the merge should
// import a and c (S3-only) and delete x (DB-only).
func TestReconcileBackend_HappyPathImportsAndDeletes(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackendKeyAsc(gomock.Any(), "b1", gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, afterKey string, _ int) ([]core.ObjectLocation, error) {
			if afterKey == "" {
				return []core.ObjectLocation{
					{ObjectKey: "vb/b", BackendName: "b1"},
					{ObjectKey: "vb/x", BackendName: "b1"},
				}, nil
			}
			return nil, nil
		}).
		AnyTimes()
	store.EXPECT().ImportObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(true, nil).
		AnyTimes()
	var deletedKeys []string
	store.EXPECT().DeleteObjectLocation(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, key, _ string) error {
			deletedKeys = append(deletedKeys, key)
			return nil
		}).
		AnyTimes()
	storetest.Permissive(store)

	listing := &listingMockBackend{
		mockBackend: newMockBackend(),
		pages: [][]backend.ListedObject{
			{{Key: "vb/a", SizeBytes: 1}, {Key: "vb/b", SizeBytes: 2}, {Key: "vb/c", SizeBytes: 3}},
		},
	}
	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": listing.mockBackend})
	// Replace the registered backend with our listing-capable variant.
	mgr.Backends()["b1"] = listing

	res, err := mgr.ReconcileBackend(context.Background(), "b1", "vb", []string{"vb"})
	if err != nil {
		t.Fatalf("ReconcileBackend: %v", err)
	}
	if res.BackendsScanned != 1 {
		t.Errorf("BackendsScanned = %d, want 1", res.BackendsScanned)
	}
	if res.Imported != 2 {
		t.Errorf("Imported = %d, want 2 (a, c)", res.Imported)
	}
	if res.Removed != 1 {
		t.Errorf("Removed = %d, want 1 (x)", res.Removed)
	}
	if len(deletedKeys) != 1 || deletedKeys[0] != "vb/x" {
		t.Errorf("delete calls = %v, want one call for vb/x", deletedKeys)
	}
}

// TestReconcileBackend_NoMutationsWhenInSync verifies the no-op case:
// every S3 key matches a DB row, so no imports or deletes fire.
func TestReconcileBackend_NoMutationsWhenInSync(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListObjectsByBackendKeyAsc(gomock.Any(), "b1", gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, afterKey string, _ int) ([]core.ObjectLocation, error) {
			if afterKey == "" {
				return []core.ObjectLocation{
					{ObjectKey: "vb/a"},
					{ObjectKey: "vb/b"},
				}, nil
			}
			return nil, nil
		}).
		AnyTimes()
	storetest.Permissive(store)

	listing := &listingMockBackend{
		mockBackend: newMockBackend(),
		pages: [][]backend.ListedObject{
			{{Key: "vb/a", SizeBytes: 1}, {Key: "vb/b", SizeBytes: 2}},
		},
	}
	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": listing.mockBackend})
	mgr.Backends()["b1"] = listing

	res, err := mgr.ReconcileBackend(context.Background(), "b1", "vb", []string{"vb"})
	if err != nil {
		t.Fatalf("ReconcileBackend: %v", err)
	}
	if res.Imported != 0 || res.Removed != 0 {
		t.Errorf("res = %+v, want zero imports + deletes", res)
	}
}

// TestReconcileBackend_PropagatesS3ListingError surfaces a transport
// failure from the listing path back to the caller.
func TestReconcileBackend_PropagatesS3ListingError(t *testing.T) {
	t.Parallel()
	want := errors.New("list boom")
	listing := &listingMockBackend{
		mockBackend: newMockBackend(),
		pages:       [][]backend.ListedObject{{{Key: "vb/a", SizeBytes: 1}}},
		err:         want,
	}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	storetest.Permissive(store)
	mgr := newTestManager(t, store, map[string]*mockBackend{"b1": listing.mockBackend})
	mgr.Backends()["b1"] = listing

	_, err := mgr.ReconcileBackend(context.Background(), "b1", "vb", []string{"vb"})
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

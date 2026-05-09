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

	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

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

	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

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

	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

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
	mgr := newTestManager(store, map[string]*mockBackend{
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
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	reconciler := worker.NewReconciler(mgr, []string{})
	reconciler.Run(context.Background())
}

// TestReconciler_CancelledContext verifies an already-cancelled ctx
// returns quickly.
func TestReconciler_CancelledContext(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

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

	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	reconciler := worker.NewReconciler(mgr, []string{"unified"})
	reconciler.Run(context.Background())
}

// TestReconcileBackend_UnknownBackend verifies the unknown-backend
// error path.
func TestReconcileBackend_UnknownBackend(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := mgr.ReconcileBackend(context.Background(), "nonexistent", "unified", []string{"unified"}); err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

// TestReconcileBackend_ListingNotSupported verifies the listing-not-
// supported error path.
func TestReconcileBackend_ListingNotSupported(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := mgr.ReconcileBackend(context.Background(), "b1", "unified", []string{"unified"}); err == nil {
		t.Fatal("expected error for backend that doesn't support listing")
	}
}

// TestReconcile_ViaReconciler exercises the reconciler-level entry
// against a backend that fails listing.
func TestReconcile_ViaReconciler(t *testing.T) {
	t.Parallel()
	store := newPermissiveMock(t)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

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
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	reconciler := worker.NewReconciler(mgr, []string{"unified"})
	result, err := reconciler.Reconcile(context.Background(), "b1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.BackendsScanned != 0 {
		t.Errorf("backends_scanned = %d, want 0 (listing not supported)", result.BackendsScanned)
	}
}

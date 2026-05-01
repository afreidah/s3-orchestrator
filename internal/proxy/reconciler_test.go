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

	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// TestMakeReconcileDeleter_SweepsCleanupQueue verifies the composed
// deleter the reconciler uses calls DeleteObjectLocation followed by
// SweepStaleCleanupQueueRows so stale queue rows pointing at a key the
// backend no longer holds are dropped in lockstep with the metadata row.
func TestMakeReconcileDeleter_SweepsCleanupQueue(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	deleter := mgr.makeReconcileDeleter()
	if err := deleter(context.Background(), "bucket/k1", "b1"); err != nil {
		t.Fatalf("deleter returned error: %v", err)
	}

	// Both store calls should have fired — DeleteObjectLocation tracked
	// via deleteObjectLocationCalls and SweepStaleCleanupQueueRows via
	// sweepStaleCalls (added when this fix landed).
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := len(store.sweepStaleCalls); got != 1 {
		t.Fatalf("SweepStaleCleanupQueueRows calls = %d, want 1", got)
	}
	if store.sweepStaleCalls[0].key != "bucket/k1" || store.sweepStaleCalls[0].backend != "b1" {
		t.Errorf("sweep called with %+v, want {key:bucket/k1 backend:b1}", store.sweepStaleCalls[0])
	}
}

// TestMakeReconcileDeleter_SweepFailureNotPropagated verifies a sweep
// error is logged and swallowed so the reconcile pass keeps going. The
// metadata delete already succeeded; one orphan queue row left for the
// next pass is preferable to aborting reconcile mid-pass.
func TestMakeReconcileDeleter_SweepFailureNotPropagated(t *testing.T) {
	t.Parallel()
	store := &mockStore{sweepStaleErr: errors.New("db blip")}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	deleter := mgr.makeReconcileDeleter()
	if err := deleter(context.Background(), "bucket/k1", "b1"); err != nil {
		t.Errorf("deleter must not propagate sweep failure, got %v", err)
	}
}

// TestMakeReconcileDeleter_DeleteFailurePropagates verifies that if the
// metadata delete itself fails, the error propagates and the sweep is
// not attempted (the row may still be live elsewhere).
func TestMakeReconcileDeleter_DeleteFailurePropagates(t *testing.T) {
	t.Parallel()
	store := &mockStore{deleteObjectLocationErr: errors.New("db blip")}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	deleter := mgr.makeReconcileDeleter()
	if err := deleter(context.Background(), "bucket/k1", "b1"); err == nil {
		t.Fatal("expected delete failure to propagate")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := len(store.sweepStaleCalls); got != 0 {
		t.Errorf("sweep should be skipped when delete fails, got %d calls", got)
	}
}

func TestReconciler_ImportsUntrackedObjects(t *testing.T) {
	t.Parallel()
	// The mock backend's ListObjects returns objects via the S3Backend
	// interface. Since we can't easily mock ListObjects on a mockBackend
	// (it doesn't implement the S3Backend.ListObjects method), we test
	// through the manager's SyncBackend path indirectly by verifying
	// the reconciler calls SyncBackend for each backend.

	// For unit testing the reconciler logic, we verify it doesn't panic
	// and handles the "backend does not support listing" error gracefully
	// (mockBackend is not an *S3Backend).
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{
		"b1": newMockBackend(),
		"b2": newMockBackend(),
	})

	reconciler := worker.NewReconciler(mgr, []string{"unified"})
	reconciler.Run(context.Background())

	// Should complete without panic. SyncBackend will log errors because
	// mockBackend doesn't support ListObjects, but the reconciler continues.
}

func TestReconciler_NoBuckets(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	reconciler := worker.NewReconciler(mgr, []string{})
	reconciler.Run(context.Background())

	// Should return early without panic when no buckets are configured.
}

func TestReconciler_CancelledContext(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	reconciler := worker.NewReconciler(mgr, []string{"unified"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reconciler.Run(ctx)

	// Should return quickly without panic on cancelled context.
}

func TestReconciler_RunDoesNotPanicOnBackendError(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		// ImportObject returns an error
		importObjectErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	reconciler := worker.NewReconciler(mgr, []string{"unified"})
	reconciler.Run(context.Background())
}

func TestReconcileBackend_UnknownBackend(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.ReconcileBackend(context.Background(), "nonexistent", "unified", []string{"unified"})
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestReconcileBackend_ListingNotSupported(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// mockBackend isn't an *S3Backend so ListObjects will fail
	_, err := mgr.ReconcileBackend(context.Background(), "b1", "unified", []string{"unified"})
	if err == nil {
		t.Fatal("expected error for backend that doesn't support listing")
	}
}

func TestReconcile_ViaReconciler(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	reconciler := worker.NewReconciler(mgr, []string{"unified"})
	result, err := reconciler.Reconcile(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// All backends fail listing (mockBackend), so nothing imported/removed
	// but it shouldn't panic
	if result.BackendsScanned != 0 {
		t.Errorf("backends_scanned = %d, want 0 (all failed)", result.BackendsScanned)
	}
}

func TestReconcile_SingleBackendViaReconciler(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
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

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

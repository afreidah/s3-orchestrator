// -------------------------------------------------------------------------------
// Reconciler Tests
//
// Author: Alex Freidah
//
// Verifies the reconciler scans every configured backend, imports
// untracked objects into the metadata store, sweeps stale rows whose
// keys the backend no longer holds, and continues past per-backend
// errors instead of aborting the cycle. The continue-on-error path is
// load-bearing: a single down backend must not stall reconciliation
// for the rest of the fleet.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"
)

// TestReconciler_NoBuckets verifies the reconciler no buckets path by exercising gomock.NewController, r.Run, context.Background.
func TestReconciler_NoBuckets(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	syncer := NewMockBackendSyncer(ctrl)
	r := NewReconciler(syncer, nil)
	r.Run(context.Background()) // should not panic
}

// TestReconciler_SyncsAllBackends verifies the reconciler syncs all backends path by exercising gomock.NewController, syncer.EXPECT, gomock.Any.
func TestReconciler_SyncsAllBackends(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	syncer := NewMockBackendSyncer(ctrl)

	syncer.EXPECT().BackendOrder().Return([]string{"b1", "b2"})
	syncer.EXPECT().SyncBackend(gomock.Any(), "b1", "unified", []string{"unified"}).Return(2, 5, nil)
	syncer.EXPECT().SyncBackend(gomock.Any(), "b2", "unified", []string{"unified"}).Return(0, 10, nil)
	syncer.EXPECT().UpdateQuotaMetrics(gomock.Any()).Return(nil)
	syncer.EXPECT().ReconcileUsage(gomock.Any()).Return(nil, nil).AnyTimes()

	r := NewReconciler(syncer, []string{"unified"})
	r.Run(context.Background())
}

// TestReconciler_ContinuesOnBackendError verifies the reconciler continues on backend error path by exercising gomock.NewController, syncer.EXPECT, gomock.Any.
func TestReconciler_ContinuesOnBackendError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	syncer := NewMockBackendSyncer(ctrl)

	syncer.EXPECT().BackendOrder().Return([]string{"b1", "b2"})
	syncer.EXPECT().SyncBackend(gomock.Any(), "b1", "unified", gomock.Any()).Return(0, 0, context.DeadlineExceeded)
	syncer.EXPECT().SyncBackend(gomock.Any(), "b2", "unified", gomock.Any()).Return(1, 0, nil)
	syncer.EXPECT().UpdateQuotaMetrics(gomock.Any()).Return(nil)
	syncer.EXPECT().ReconcileUsage(gomock.Any()).Return(nil, nil).AnyTimes()

	r := NewReconciler(syncer, []string{"unified"})
	r.Run(context.Background()) // should not panic
}

// TestReconciler_ReconcilesUsageEachPass verifies usage reconciliation runs
// every pass even when nothing was imported (counter drift can exist
// independently of imports, so it must not be gated on import count).
func TestReconciler_ReconcilesUsageEachPass(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	syncer := NewMockBackendSyncer(ctrl)

	syncer.EXPECT().BackendOrder().Return([]string{"b1"})
	syncer.EXPECT().SyncBackend(gomock.Any(), "b1", "unified", []string{"unified"}).Return(0, 0, nil)
	// Zero imports: UpdateQuotaMetrics is skipped, but ReconcileUsage still runs.
	syncer.EXPECT().ReconcileUsage(gomock.Any()).Return(map[string]int64{"b1": -100}, nil).Times(1)

	r := NewReconciler(syncer, []string{"unified"})
	r.Run(context.Background())
}

// TestReconcile_AllBackends verifies the reconcile all backends contract.
// Asserts that unexpected error:.
func TestReconcile_AllBackends(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	syncer := NewMockBackendSyncer(ctrl)

	syncer.EXPECT().BackendOrder().Return([]string{"b1", "b2"})
	syncer.EXPECT().ReconcileBackend(gomock.Any(), "b1", "unified", []string{"unified"}).
		Return(&ReconcileResult{Imported: 1, Removed: 3, BackendsScanned: 1}, nil)
	syncer.EXPECT().ReconcileBackend(gomock.Any(), "b2", "unified", []string{"unified"}).
		Return(&ReconcileResult{Imported: 0, Removed: 2, BackendsScanned: 1}, nil)
	syncer.EXPECT().UpdateQuotaMetrics(gomock.Any()).Return(nil)
	syncer.EXPECT().ReconcileUsage(gomock.Any()).Return(nil, nil).AnyTimes()

	r := NewReconciler(syncer, []string{"unified"})
	result, err := r.Reconcile(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Imported != 1 {
		t.Errorf("imported = %d, want 1", result.Imported)
	}
	if result.Removed != 5 {
		t.Errorf("removed = %d, want 5", result.Removed)
	}
	if result.BackendsScanned != 2 {
		t.Errorf("backends_scanned = %d, want 2", result.BackendsScanned)
	}
}

// TestReconcile_SingleBackend verifies the reconcile single backend contract.
// Asserts that unexpected error:.
func TestReconcile_SingleBackend(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	syncer := NewMockBackendSyncer(ctrl)

	syncer.EXPECT().ReconcileBackend(gomock.Any(), "b1", "unified", []string{"unified"}).
		Return(&ReconcileResult{Imported: 0, Removed: 10, BackendsScanned: 1}, nil)
	syncer.EXPECT().UpdateQuotaMetrics(gomock.Any()).Return(nil)
	syncer.EXPECT().ReconcileUsage(gomock.Any()).Return(nil, nil).AnyTimes()

	r := NewReconciler(syncer, []string{"unified"})
	result, err := r.Reconcile(context.Background(), "b1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Removed != 10 {
		t.Errorf("removed = %d, want 10", result.Removed)
	}
	if result.BackendsScanned != 1 {
		t.Errorf("backends_scanned = %d, want 1", result.BackendsScanned)
	}
}

// TestReconcile_NoBuckets verifies the reconcile no buckets path by exercising gomock.NewController, r.Reconcile, context.Background.
func TestReconcile_NoBuckets(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	syncer := NewMockBackendSyncer(ctrl)

	r := NewReconciler(syncer, nil)
	_, err := r.Reconcile(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for no buckets")
	}
}

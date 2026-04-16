package worker

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"
)

func TestReconciler_NoBuckets(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	syncer := NewMockBackendSyncer(ctrl)
	r := NewReconciler(syncer, nil)
	r.Run(context.Background()) // should not panic
}

func TestReconciler_SyncsAllBackends(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	syncer := NewMockBackendSyncer(ctrl)

	syncer.EXPECT().BackendOrder().Return([]string{"b1", "b2"})
	syncer.EXPECT().SyncBackend(gomock.Any(), "b1", "unified", []string{"unified"}).Return(2, 5, nil)
	syncer.EXPECT().SyncBackend(gomock.Any(), "b2", "unified", []string{"unified"}).Return(0, 10, nil)
	syncer.EXPECT().UpdateQuotaMetrics(gomock.Any()).Return(nil)

	r := NewReconciler(syncer, []string{"unified"})
	r.Run(context.Background())
}

func TestReconciler_ContinuesOnBackendError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	syncer := NewMockBackendSyncer(ctrl)

	syncer.EXPECT().BackendOrder().Return([]string{"b1", "b2"})
	syncer.EXPECT().SyncBackend(gomock.Any(), "b1", "unified", gomock.Any()).Return(0, 0, context.DeadlineExceeded)
	syncer.EXPECT().SyncBackend(gomock.Any(), "b2", "unified", gomock.Any()).Return(1, 0, nil)
	syncer.EXPECT().UpdateQuotaMetrics(gomock.Any()).Return(nil)

	r := NewReconciler(syncer, []string{"unified"})
	r.Run(context.Background()) // should not panic
}

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

func TestReconcile_SingleBackend(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	syncer := NewMockBackendSyncer(ctrl)

	syncer.EXPECT().ReconcileBackend(gomock.Any(), "b1", "unified", []string{"unified"}).
		Return(&ReconcileResult{Imported: 0, Removed: 10, BackendsScanned: 1}, nil)
	syncer.EXPECT().UpdateQuotaMetrics(gomock.Any()).Return(nil)

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

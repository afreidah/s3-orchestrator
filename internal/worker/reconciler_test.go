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

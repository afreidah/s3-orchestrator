package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store"
	"go.uber.org/mock/gomock"
)

func TestProcessCleanupQueue_DeleteSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockCleanupDeps(ctrl)

	st := store.CleanupItem{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt", SizeBytes: 100}
	ms := &mockMetadataStore{pendingCleanups: []store.CleanupItem{st}}

	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(nil, nil) // backend value doesn't matter for this test
	ops.EXPECT().DeleteWithTimeout(gomock.Any(), gomock.Any(), "orphan.txt").Return(nil)
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()

	w := NewCleanupWorker(ops, 1)
	processed, failed := w.ProcessCleanupQueue(context.Background())

	if processed != 1 {
		t.Errorf("expected processed=1, got %d", processed)
	}
	if failed != 0 {
		t.Errorf("expected failed=0, got %d", failed)
	}
}

func TestProcessCleanupQueue_DeleteFails_Retries(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockCleanupDeps(ctrl)

	st := store.CleanupItem{ID: 2, BackendName: "b1", ObjectKey: "stuck.txt", Attempts: 3}
	ms := &mockMetadataStore{pendingCleanups: []store.CleanupItem{st}}

	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("b1").Return(nil, nil)
	ops.EXPECT().DeleteWithTimeout(gomock.Any(), gomock.Any(), "stuck.txt").Return(errors.New("timeout"))
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()

	w := NewCleanupWorker(ops, 1)
	_, failed := w.ProcessCleanupQueue(context.Background())

	if failed != 1 {
		t.Errorf("expected failed=1, got %d", failed)
	}
}

func TestProcessCleanupQueue_AdmissionBlocked(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockCleanupDeps(ctrl)

	st := store.CleanupItem{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt"}
	ms := &mockMetadataStore{pendingCleanups: []store.CleanupItem{st}}

	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(false)

	w := NewCleanupWorker(ops, 1)
	processed, failed := w.ProcessCleanupQueue(context.Background())

	if processed != 0 || failed != 0 {
		t.Errorf("expected 0/0 when blocked, got %d/%d", processed, failed)
	}
}

func TestProcessCleanupQueue_BackendNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockCleanupDeps(ctrl)

	st := store.CleanupItem{ID: 1, BackendName: "gone", ObjectKey: "orphan.txt"}
	ms := &mockMetadataStore{pendingCleanups: []store.CleanupItem{st}}

	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().AcquireAdmission(gomock.Any()).Return(true)
	ops.EXPECT().ReleaseAdmission()
	ops.EXPECT().GetBackend("gone").Return(nil, errors.New("not found"))
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()

	w := NewCleanupWorker(ops, 1)
	processed, _ := w.ProcessCleanupQueue(context.Background())

	if processed != 1 {
		t.Errorf("expected processed=1 (item removed), got %d", processed)
	}
}

func TestCleanupBackoff(t *testing.T) {
	tests := []struct {
		attempts int32
		want     time.Duration
	}{
		{0, 1 * time.Minute},
		{1, 2 * time.Minute},
		{5, 32 * time.Minute},
		{11, 24 * time.Hour},
	}
	for _, tt := range tests {
		got := CleanupBackoff(tt.attempts)
		if got != tt.want {
			t.Errorf("CleanupBackoff(%d) = %v, want %v", tt.attempts, got, tt.want)
		}
	}
}

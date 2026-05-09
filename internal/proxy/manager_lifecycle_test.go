// -------------------------------------------------------------------------------
// Lifecycle Operations Tests
//
// Author: Alex Freidah
//
// Tests for BackendManager lifecycle operations: expired object cleanup, stale
// multipart upload abortion, and temporary object deletion. Validates store
// queries and backend interaction for each lifecycle phase.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// expiredLister returns a DoAndReturn closure that hands out one page of
// expired-object rows per call, then nil to terminate. Mirrors the
// pagination contract ListExpiredObjects has against the production store.
func expiredLister(pages [][]core.ObjectLocation) func(context.Context, string, time.Time, int) ([]core.ObjectLocation, error) {
	idx := 0
	return func(_ context.Context, _ string, _ time.Time, _ int) ([]core.ObjectLocation, error) {
		if idx >= len(pages) {
			return nil, nil
		}
		page := pages[idx]
		idx++
		return page, nil
	}
}

// captureDeletes returns a DoAndReturn closure that records each
// DeleteObject key into the supplied slice and returns the configured
// response or error.
func captureDeletes(into *[]string, resp []core.DeletedCopy, err error) func(context.Context, string) ([]core.DeletedCopy, error) {
	return func(_ context.Context, key string) ([]core.DeletedCopy, error) {
		*into = append(*into, key)
		if err != nil {
			return nil, err
		}
		return resp, nil
	}
}

// TestProcessLifecycleRules_DeletesExpiredObjects verifies the lifecycle
// rule processor deletes a single expired object end-to-end.
func TestProcessLifecycleRules_DeletesExpiredObjects(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.objects["tmp/old-file"] = mockObject{data: []byte("data")}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListExpiredObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(expiredLister([][]core.ObjectLocation{
			{{ObjectKey: "tmp/old-file", BackendName: "b1", SizeBytes: 4, CreatedAt: time.Now().Add(-48 * time.Hour)}},
		})).
		AnyTimes()
	var deletes []string
	store.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
		DoAndReturn(captureDeletes(&deletes, []core.DeletedCopy{{BackendName: "b1", SizeBytes: 4}}, nil)).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 1},
	})
	if deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", deleted)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
	if len(deletes) != 1 || deletes[0] != "tmp/old-file" {
		t.Errorf("delete calls = %v, want [tmp/old-file]", deletes)
	}
}

// TestProcessLifecycleRules_NoExpiredObjects verifies the no-op case.
func TestProcessLifecycleRules_NoExpiredObjects(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	var deletes []string
	store.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
		DoAndReturn(captureDeletes(&deletes, nil, nil)).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 7},
	})
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
	if len(deletes) != 0 {
		t.Errorf("expected 0 DeleteObject calls, got %d", len(deletes))
	}
}

// TestProcessLifecycleRules_MultipleRules confirms a rules slice with
// distinct prefixes runs each rule once.
func TestProcessLifecycleRules_MultipleRules(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.objects["tmp/a"] = mockObject{data: []byte("x")}
	backend.objects["uploads/staging/b"] = mockObject{data: []byte("y")}

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListExpiredObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(expiredLister([][]core.ObjectLocation{
			{{ObjectKey: "tmp/a", BackendName: "b1", SizeBytes: 1, CreatedAt: time.Now().Add(-48 * time.Hour)}},
			{{ObjectKey: "uploads/staging/b", BackendName: "b1", SizeBytes: 1, CreatedAt: time.Now().Add(-3 * time.Hour)}},
		})).
		AnyTimes()
	store.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}, nil).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 1},
		{Prefix: "uploads/staging/", ExpirationDays: 1},
	})
	if deleted != 2 {
		t.Errorf("expected 2 deleted, got %d", deleted)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
}

// TestProcessLifecycleRules_BatchPagination drives a full batch then an
// empty page to exercise the inner pagination loop.
func TestProcessLifecycleRules_BatchPagination(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	const defaultBatchSize = 100
	batch := make([]core.ObjectLocation, defaultBatchSize)
	for i := range batch {
		key := "tmp/" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		batch[i] = core.ObjectLocation{ObjectKey: key, BackendName: "b1", SizeBytes: 1, CreatedAt: time.Now().Add(-48 * time.Hour)}
		backend.objects[key] = mockObject{data: []byte("x")}
	}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListExpiredObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(expiredLister([][]core.ObjectLocation{batch, nil})).
		AnyTimes()
	store.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}, nil).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})
	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 1},
	})
	if deleted != defaultBatchSize {
		t.Errorf("expected %d deleted, got %d", defaultBatchSize, deleted)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
}

// TestProcessLifecycleRules_DeleteFailureContinues verifies that a failed
// DeleteObject increments the failed counter without aborting the batch.
func TestProcessLifecycleRules_DeleteFailureContinues(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListExpiredObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(expiredLister([][]core.ObjectLocation{
			{
				{ObjectKey: "tmp/a", BackendName: "b1", SizeBytes: 1},
				{ObjectKey: "tmp/b", BackendName: "b1", SizeBytes: 1},
			},
		})).
		AnyTimes()
	var deletes []string
	store.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
		DoAndReturn(captureDeletes(&deletes, nil, errors.New("backend unreachable"))).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})
	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 1},
	})
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}
	if failed != 2 {
		t.Errorf("expected 2 failed, got %d", failed)
	}
	if len(deletes) != 2 {
		t.Errorf("expected 2 DeleteObject calls, got %d", len(deletes))
	}
}

// TestProcessLifecycleRules_ListExpiredObjectsError surfaces a store-side
// listing failure as a single failed-rule entry.
func TestProcessLifecycleRules_ListExpiredObjectsError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListExpiredObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})
	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 7},
	})
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}
	if failed != 1 {
		t.Errorf("expected 1 failed, got %d", failed)
	}
}

// TestProcessLifecycleRules_ZeroProgressTerminates pins the
// no-forward-progress guard: a full batch returning all-failures must not
// loop forever.
func TestProcessLifecycleRules_ZeroProgressTerminates(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	const batchSize = 100
	batch := make([]core.ObjectLocation, batchSize)
	for i := range batch {
		batch[i] = core.ObjectLocation{
			ObjectKey:   fmt.Sprintf("tmp/%04d", i),
			BackendName: "b1",
			SizeBytes:   1,
			CreatedAt:   time.Now().Add(-48 * time.Hour),
		}
	}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().ListExpiredObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(expiredLister([][]core.ObjectLocation{batch, batch})).
		AnyTimes()
	store.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("backend unreachable")).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})
	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 1},
	})
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}
	if failed != batchSize {
		t.Errorf("expected %d failed, got %d", batchSize, failed)
	}
}

// TestProcessLifecycleRules_EmptyRulesNoOp asserts an empty rules slice
// makes no store calls.
func TestProcessLifecycleRules_EmptyRulesNoOp(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	storetest.Permissive(store)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), nil)
	if deleted != 0 || failed != 0 {
		t.Errorf("expected no-op, got deleted=%d failed=%d", deleted, failed)
	}
}

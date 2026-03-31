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

	st "github.com/afreidah/s3-orchestrator/internal/store"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

func TestProcessLifecycleRules_DeletesExpiredObjects(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.objects["tmp/old-file"] = mockObject{data: []byte("data")}
	store := &mockStore{
		listExpiredObjectsResp: []st.ObjectLocation{
			{ObjectKey: "tmp/old-file", BackendName: "b1", SizeBytes: 4, CreatedAt: time.Now().Add(-48 * time.Hour)},
		},
		deleteObjectResp: []st.DeletedCopy{{BackendName: "b1", SizeBytes: 4}},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	rules := []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 1},
	}

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), rules)
	if deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", deleted)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
	if len(store.deleteObjectCalls) != 1 {
		t.Fatalf("expected 1 DeleteObject call, got %d", len(store.deleteObjectCalls))
	}
	if store.deleteObjectCalls[0] != "tmp/old-file" {
		t.Errorf("expected delete of 'tmp/old-file', got %q", store.deleteObjectCalls[0])
	}
}

func TestProcessLifecycleRules_NoExpiredObjects(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		listExpiredObjectsResp: nil, // no expired objects
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	rules := []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 7},
	}

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), rules)
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
	if len(store.deleteObjectCalls) != 0 {
		t.Errorf("expected 0 DeleteObject calls, got %d", len(store.deleteObjectCalls))
	}
}

func TestProcessLifecycleRules_MultipleRules(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.objects["tmp/a"] = mockObject{data: []byte("x")}
	backend.objects["uploads/staging/b"] = mockObject{data: []byte("y")}

	store := &mockStore{
		deleteObjectResp: []st.DeletedCopy{{BackendName: "b1", SizeBytes: 1}},
	}
	// Each rule's batch is under lifecycleBatchSize, so the loop breaks without
	// a second call per rule — no nil terminators needed.
	store.listExpiredObjectsPages = [][]st.ObjectLocation{
		{{ObjectKey: "tmp/a", BackendName: "b1", SizeBytes: 1, CreatedAt: time.Now().Add(-48 * time.Hour)}},
		{{ObjectKey: "uploads/staging/b", BackendName: "b1", SizeBytes: 1, CreatedAt: time.Now().Add(-3 * time.Hour)}},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	rules := []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 1},
		{Prefix: "uploads/staging/", ExpirationDays: 1},
	}

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), rules)
	if deleted != 2 {
		t.Errorf("expected 2 deleted, got %d", deleted)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
}

func TestProcessLifecycleRules_BatchPagination(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{
		deleteObjectResp: []st.DeletedCopy{{BackendName: "b1", SizeBytes: 1}},
	}

	// Build a batch of exactly 100 objects (default batch size), then an empty page
	const defaultBatchSize = 100
	batch := make([]st.ObjectLocation, defaultBatchSize)
	for i := range batch {
		key := "tmp/" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		batch[i] = st.ObjectLocation{ObjectKey: key, BackendName: "b1", SizeBytes: 1, CreatedAt: time.Now().Add(-48 * time.Hour)}
		backend.objects[key] = mockObject{data: []byte("x")}
	}
	store.listExpiredObjectsPages = [][]st.ObjectLocation{
		batch,
		nil, // second page empty = done
	}

	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	rules := []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 1},
	}

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), rules)
	if deleted != defaultBatchSize {
		t.Errorf("expected %d deleted, got %d", defaultBatchSize, deleted)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
}

func TestProcessLifecycleRules_DeleteFailureContinues(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{
		listExpiredObjectsResp: []st.ObjectLocation{
			{ObjectKey: "tmp/a", BackendName: "b1", SizeBytes: 1},
			{ObjectKey: "tmp/b", BackendName: "b1", SizeBytes: 1},
		},
		// DeleteObject will fail for all keys
		deleteObjectErr: errors.New("backend unreachable"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	rules := []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 1},
	}

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), rules)
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}
	if failed != 2 {
		t.Errorf("expected 2 failed, got %d", failed)
	}
	// Both objects should have been attempted
	if len(store.deleteObjectCalls) != 2 {
		t.Errorf("expected 2 DeleteObject calls, got %d", len(store.deleteObjectCalls))
	}
}

func TestProcessLifecycleRules_ListExpiredObjectsError(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		listExpiredObjectsErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	rules := []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 7},
	}

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), rules)
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}
	if failed != 1 {
		t.Errorf("expected 1 failed, got %d", failed)
	}
}

func TestProcessLifecycleRules_ZeroProgressTerminates(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{
		// Return a full batch (100 objects) that all fail to delete.
		// Without the zero-progress guard this would loop forever.
		deleteObjectErr: errors.New("backend unreachable"),
	}

	// Build a batch of exactly 100 objects (default batch size)
	const batchSize = 100
	batch := make([]st.ObjectLocation, batchSize)
	for i := range batch {
		batch[i] = st.ObjectLocation{
			ObjectKey:   fmt.Sprintf("tmp/%04d", i),
			BackendName: "b1",
			SizeBytes:   1,
			CreatedAt:   time.Now().Add(-48 * time.Hour),
		}
	}

	// Return the full batch on every call — the guard must break the loop.
	store.listExpiredObjectsPages = [][]st.ObjectLocation{
		batch,
		batch, // second page would be fetched if the guard didn't stop
	}

	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	rules := []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 1},
	}

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), rules)
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}
	if failed != batchSize {
		t.Errorf("expected %d failed, got %d", batchSize, failed)
	}
}

func TestProcessLifecycleRules_EmptyRulesNoOp(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), nil)
	if deleted != 0 || failed != 0 {
		t.Errorf("expected no-op, got deleted=%d failed=%d", deleted, failed)
	}
}

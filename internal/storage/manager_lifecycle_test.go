// -------------------------------------------------------------------------------
// Lifecycle Operations Tests
//
// Author: Alex Freidah
//
// Tests for BackendManager lifecycle operations: expired object cleanup, stale
// multipart upload abortion, and temporary object deletion. Validates store
// queries and backend interaction for each lifecycle phase.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

func TestProcessLifecycleRules_DeletesExpiredObjects(t *testing.T) {
	backend := newMockBackend()
	backend.objects["tmp/old-file"] = mockObject{data: []byte("data")}
	store := &mockStore{
		listExpiredObjectsResp: []ObjectLocation{
			{ObjectKey: "tmp/old-file", BackendName: "b1", SizeBytes: 4, CreatedAt: time.Now().Add(-48 * time.Hour)},
		},
		deleteObjectResp: []DeletedCopy{{BackendName: "b1", SizeBytes: 4}},
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
	backend := newMockBackend()
	backend.objects["tmp/a"] = mockObject{data: []byte("x")}
	backend.objects["uploads/staging/b"] = mockObject{data: []byte("y")}

	store := &mockStore{
		deleteObjectResp: []DeletedCopy{{BackendName: "b1", SizeBytes: 1}},
	}
	// Each rule's batch is under lifecycleBatchSize, so the loop breaks without
	// a second call per rule — no nil terminators needed.
	store.listExpiredObjectsPages = [][]ObjectLocation{
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
	backend := newMockBackend()
	store := &mockStore{
		deleteObjectResp: []DeletedCopy{{BackendName: "b1", SizeBytes: 1}},
	}

	// Build a batch of exactly lifecycleBatchSize objects, then an empty page
	batch := make([]ObjectLocation, lifecycleBatchSize)
	for i := range batch {
		key := "tmp/" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		batch[i] = ObjectLocation{ObjectKey: key, BackendName: "b1", SizeBytes: 1, CreatedAt: time.Now().Add(-48 * time.Hour)}
		backend.objects[key] = mockObject{data: []byte("x")}
	}
	store.listExpiredObjectsPages = [][]ObjectLocation{
		batch,
		nil, // second page empty = done
	}

	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	rules := []config.LifecycleRule{
		{Prefix: "tmp/", ExpirationDays: 1},
	}

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), rules)
	if deleted != lifecycleBatchSize {
		t.Errorf("expected %d deleted, got %d", lifecycleBatchSize, deleted)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
}

func TestProcessLifecycleRules_DeleteFailureContinues(t *testing.T) {
	backend := newMockBackend()
	store := &mockStore{
		listExpiredObjectsResp: []ObjectLocation{
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

func TestProcessLifecycleRules_EmptyRulesNoOp(t *testing.T) {
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	deleted, failed := mgr.ProcessLifecycleRules(context.Background(), nil)
	if deleted != 0 || failed != 0 {
		t.Errorf("expected no-op, got deleted=%d failed=%d", deleted, failed)
	}
}

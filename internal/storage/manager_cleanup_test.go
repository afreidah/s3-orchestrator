// -------------------------------------------------------------------------------
// Cleanup Queue Manager Tests
//
// Author: Alex Freidah
//
// Tests for the cleanup retry worker: exponential backoff calculation, queue
// processing with successful and failed retries, maximum attempt enforcement,
// and best-effort enqueue behavior during database outages.
// -------------------------------------------------------------------------------

package storage

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// cleanupBackoff
// -------------------------------------------------------------------------

func TestCleanupBackoff(t *testing.T) {
	tests := []struct {
		attempts int32
		want     time.Duration
	}{
		{0, 1 * time.Minute},
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{4, 16 * time.Minute},
		{5, 32 * time.Minute},
		{6, 64 * time.Minute},
		{7, 128 * time.Minute},
		{8, 256 * time.Minute},
		{9, 512 * time.Minute},
		{10, 1024 * time.Minute},
		{11, 24 * time.Hour}, // capped at 24h
		{15, 24 * time.Hour}, // still capped
	}
	for _, tt := range tests {
		got := cleanupBackoff(tt.attempts)
		if got != tt.want {
			t.Errorf("cleanupBackoff(%d) = %v, want %v", tt.attempts, got, tt.want)
		}
	}
}

// -------------------------------------------------------------------------
// enqueueCleanup
// -------------------------------------------------------------------------

func TestEnqueueCleanup_Success(t *testing.T) {
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	mgr.enqueueCleanup(context.Background(), "b1", "orphan.txt", "orphan_put")

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.enqueueCleanupCalls) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(store.enqueueCleanupCalls))
	}
	c := store.enqueueCleanupCalls[0]
	if c.backendName != "b1" || c.objectKey != "orphan.txt" || c.reason != "orphan_put" {
		t.Errorf("unexpected call: %+v", c)
	}
}

func TestEnqueueCleanup_DBError_LogsOnly(t *testing.T) {
	store := &mockStore{enqueueCleanupErr: errors.New("db down")}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Should not panic
	mgr.enqueueCleanup(context.Background(), "b1", "orphan.txt", "orphan_put")

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.enqueueCleanupCalls) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(store.enqueueCleanupCalls))
	}
}

// -------------------------------------------------------------------------
// ProcessCleanupQueue
// -------------------------------------------------------------------------

func TestProcessCleanupQueue_DeleteSuccess(t *testing.T) {
	backend := newMockBackend()
	// Pre-populate orphan on the backend
	_, _ = backend.PutObject(context.Background(), "orphan.txt", bytes.NewReader([]byte("data")), 4, "text/plain")

	store := &mockStore{
		pendingCleanups: []CleanupItem{
			{ID: 1, BackendName: "b1", ObjectKey: "orphan.txt", Reason: "orphan_put", Attempts: 0},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	processed, failed := mgr.ProcessCleanupQueue(context.Background())

	if processed != 1 {
		t.Errorf("expected processed=1, got %d", processed)
	}
	if failed != 0 {
		t.Errorf("expected failed=0, got %d", failed)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.completeCleanupCalls) != 1 || store.completeCleanupCalls[0] != 1 {
		t.Errorf("expected CompleteCleanupItem(1), got %v", store.completeCleanupCalls)
	}
	if backend.hasObject("orphan.txt") {
		t.Error("expected orphan to be deleted from backend")
	}
}

func TestProcessCleanupQueue_DeleteFails_SchedulesRetry(t *testing.T) {
	backend := newMockBackend()
	backend.delErr = errors.New("backend timeout")

	store := &mockStore{
		pendingCleanups: []CleanupItem{
			{ID: 2, BackendName: "b1", ObjectKey: "stuck.txt", Reason: "delete_failed", Attempts: 3},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	processed, failed := mgr.ProcessCleanupQueue(context.Background())

	if processed != 0 {
		t.Errorf("expected processed=0, got %d", processed)
	}
	if failed != 1 {
		t.Errorf("expected failed=1, got %d", failed)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.retryCleanupCalls) != 1 {
		t.Fatalf("expected 1 retry call, got %d", len(store.retryCleanupCalls))
	}
	rc := store.retryCleanupCalls[0]
	if rc.id != 2 {
		t.Errorf("expected retry for id=2, got %d", rc.id)
	}
	expectedBackoff := cleanupBackoff(3) // 8 minutes
	if rc.backoff != expectedBackoff {
		t.Errorf("expected backoff=%v, got %v", expectedBackoff, rc.backoff)
	}
	if rc.lastError != "backend timeout" {
		t.Errorf("expected lastError='backend timeout', got %q", rc.lastError)
	}
}

func TestProcessCleanupQueue_BackendNotFound_RemovesItem(t *testing.T) {
	store := &mockStore{
		pendingCleanups: []CleanupItem{
			{ID: 3, BackendName: "gone-backend", ObjectKey: "orphan.txt", Reason: "orphan_put", Attempts: 0},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	processed, failed := mgr.ProcessCleanupQueue(context.Background())

	if processed != 1 {
		t.Errorf("expected processed=1, got %d", processed)
	}
	if failed != 0 {
		t.Errorf("expected failed=0, got %d", failed)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.completeCleanupCalls) != 1 || store.completeCleanupCalls[0] != 3 {
		t.Errorf("expected CompleteCleanupItem(3), got %v", store.completeCleanupCalls)
	}
}

func TestProcessCleanupQueue_EmptyQueue(t *testing.T) {
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	processed, failed := mgr.ProcessCleanupQueue(context.Background())

	if processed != 0 || failed != 0 {
		t.Errorf("expected 0/0, got %d/%d", processed, failed)
	}
}

func TestProcessCleanupQueue_FetchError(t *testing.T) {
	store := &mockStore{getPendingErr: errors.New("db error")}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	processed, failed := mgr.ProcessCleanupQueue(context.Background())

	if processed != 0 || failed != 0 {
		t.Errorf("expected 0/0 on fetch error, got %d/%d", processed, failed)
	}
}

func TestProcessCleanupQueue_MaxAttemptsReached(t *testing.T) {
	backend := newMockBackend()
	backend.delErr = errors.New("backend timeout")

	store := &mockStore{
		pendingCleanups: []CleanupItem{
			{ID: 5, BackendName: "b1", ObjectKey: "stuck.txt", Reason: "delete_failed", Attempts: 9},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	processed, failed := mgr.ProcessCleanupQueue(context.Background())

	if processed != 0 {
		t.Errorf("expected processed=0, got %d", processed)
	}
	if failed != 1 {
		t.Errorf("expected failed=1, got %d", failed)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	// Exhausted items should be removed via CompleteCleanupItem, not retried
	if len(store.retryCleanupCalls) != 0 {
		t.Errorf("expected 0 retry calls for exhausted item, got %d", len(store.retryCleanupCalls))
	}
	if len(store.completeCleanupCalls) != 1 || store.completeCleanupCalls[0] != 5 {
		t.Errorf("expected CompleteCleanupItem(5), got %v", store.completeCleanupCalls)
	}
}

func TestProcessCleanupQueue_CompleteItemError(t *testing.T) {
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "orphan.txt", bytes.NewReader([]byte("data")), 4, "text/plain")

	store := &mockStore{
		pendingCleanups: []CleanupItem{
			{ID: 6, BackendName: "b1", ObjectKey: "orphan.txt", Reason: "orphan_put", Attempts: 0},
		},
		completeCleanupErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	// Should not panic despite CompleteCleanupItem error
	processed, failed := mgr.ProcessCleanupQueue(context.Background())
	if processed != 1 {
		t.Errorf("expected processed=1 (delete succeeded), got %d", processed)
	}
	if failed != 0 {
		t.Errorf("expected failed=0, got %d", failed)
	}
}

func TestProcessCleanupQueue_RetryItemError(t *testing.T) {
	backend := newMockBackend()
	backend.delErr = errors.New("backend down")

	store := &mockStore{
		pendingCleanups: []CleanupItem{
			{ID: 7, BackendName: "b1", ObjectKey: "stuck.txt", Reason: "delete_failed", Attempts: 1},
		},
		retryCleanupErr: errors.New("db error on retry"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	// Should not panic despite RetryCleanupItem error
	processed, failed := mgr.ProcessCleanupQueue(context.Background())
	if processed != 0 {
		t.Errorf("expected processed=0, got %d", processed)
	}
	if failed != 1 {
		t.Errorf("expected failed=1, got %d", failed)
	}
}

func TestProcessCleanupQueue_QueueDepthError(t *testing.T) {
	store := &mockStore{
		cleanupQueueDepthErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Should not panic — depth error is silently ignored
	processed, failed := mgr.ProcessCleanupQueue(context.Background())
	if processed != 0 || failed != 0 {
		t.Errorf("expected 0/0, got %d/%d", processed, failed)
	}
}

func TestProcessCleanupQueue_BackendNotFound_CompleteItemError(t *testing.T) {
	store := &mockStore{
		pendingCleanups: []CleanupItem{
			{ID: 8, BackendName: "gone-backend", ObjectKey: "orphan.txt", Reason: "orphan_put", Attempts: 0},
		},
		completeCleanupErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Should not panic — completion error is logged only
	processed, failed := mgr.ProcessCleanupQueue(context.Background())
	if processed != 1 {
		t.Errorf("expected processed=1, got %d", processed)
	}
	if failed != 0 {
		t.Errorf("expected failed=0, got %d", failed)
	}
}

// -------------------------------------------------------------------------
// Enqueue wiring at failure sites
// -------------------------------------------------------------------------

func TestDeleteObject_BackendDeleteFails_EnqueuesCleanup(t *testing.T) {
	backend := newMockBackend()
	store := &mockStore{
		deleteObjectResp: []DeletedCopy{{BackendName: "b1", SizeBytes: 100}},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	// Set delete error after the store has returned the copies
	backend.mu.Lock()
	backend.delErr = errors.New("backend timeout")
	backend.mu.Unlock()

	err := mgr.DeleteObject(context.Background(), "mykey")
	if err != nil {
		t.Fatalf("DeleteObject should succeed even if backend delete fails: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.enqueueCleanupCalls) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(store.enqueueCleanupCalls))
	}
	c := store.enqueueCleanupCalls[0]
	if c.backendName != "b1" || c.objectKey != "mykey" || c.reason != "delete_failed" {
		t.Errorf("unexpected enqueue call: %+v", c)
	}
}

func TestPutObject_RecordFails_OrphanDeleteFails_EnqueuesCleanup(t *testing.T) {
	backend := newMockBackend()
	backend.delErr = errors.New("delete failed too")
	store := &mockStore{
		getBackendResp:  "b1",
		recordObjectErr: errors.New("db error"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.PutObject(context.Background(), "mykey", bytes.NewReader([]byte("data")), 4, "text/plain")
	if err == nil {
		t.Fatal("expected error from PutObject")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.enqueueCleanupCalls) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(store.enqueueCleanupCalls))
	}
	c := store.enqueueCleanupCalls[0]
	if c.reason != "orphan_record_failed" {
		t.Errorf("expected reason=orphan_record_failed, got %q", c.reason)
	}
}

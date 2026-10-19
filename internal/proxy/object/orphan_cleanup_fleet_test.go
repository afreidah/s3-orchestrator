// -------------------------------------------------------------------------------
// Cleanup Settle Tests
//
// Author: Alex Freidah
//
// Validates how the request path settles the cleanup rows its transactions
// queue: displaced copies on overwrite and copies removed by a delete are
// deleted and their rows retired, a failed delete sends the row back for
// retry, and a row naming an unknown backend is retired without a delete.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// settleCalls records the outcome the settler wrote for each cleanup row.
type settleCalls struct {
	mu        sync.Mutex
	completed []int64
	retried   []int64
	discarded int
}

// outcomes copies the retired and retried row ids.
func (c *settleCalls) outcomes() (completed, retried []int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.completed), slices.Clone(c.retried)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// settleStore returns a mock store that records each row outcome, with
// extra registering the test's own expectations first.
func settleStore(t *testing.T, extra func(*storetest.MockMetadataStore)) (*storetest.MockMetadataStore, *settleCalls) {
	t.Helper()
	c := &settleCalls{}
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	extra(store)
	store.EXPECT().CompleteCleanupItems(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, items []core.CleanupItem) ([]int64, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			ids := make([]int64, len(items))
			for i := range items {
				ids[i] = items[i].ID
			}
			c.completed = append(c.completed, ids...)
			return ids, nil
		}).AnyTimes()
	store.EXPECT().RetryCleanupItem(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, item *core.CleanupItem, _ time.Duration, _ string) (bool, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.retried = append(c.retried, item.ID)
			return true, nil
		}).AnyTimes()
	store.EXPECT().DiscardPending(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, *core.PendingObject, string) ([]core.CleanupItem, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.discarded++
			return nil, nil
		}).AnyTimes()
	storetest.Permissive(store)
	return store, c
}

// displacedRow is the cleanup row an overwrite queues for the copy it
// displaced.
func displacedRow(id int64, backendName, key string, size int64) core.CleanupItem {
	return core.CleanupItem{
		ID: id, BackendName: backendName, ObjectKey: key, Reason: core.CleanupReasonOverwritten, SizeBytes: size,
	}
}

// putSmall writes a small object under key.
func putSmall(mgr *fleet, key string) error {
	_, err := mgr.PutObject(context.Background(), &PutObjectRequest{
		Key: key, Body: bytes.NewReader([]byte("new")), Size: 3, ContentType: "text/plain",
	})
	return err
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestPutObject_Overwrite_DisplacedDeleteFails_RetriesRow pins that a
// displaced copy whose delete fails keeps its queued row, sent back for retry.
func TestPutObject_Overwrite_DisplacedDeleteFails_RetriesRow(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	b2.DeleteErr = errors.New("backend down")

	store, c := settleStore(t, func(s *storetest.MockMetadataStore) {
		s.EXPECT().RecordObject(gomock.Any(), gomock.Any()).
			Return([]core.CleanupItem{displacedRow(7, "b2", "overwritten-key", 500)}, nil, nil).AnyTimes()
	})
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})

	if err := putSmall(mgr, "overwritten-key"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	completed, retried := c.outcomes()
	if len(retried) != 1 || retried[0] != 7 || len(completed) != 0 {
		t.Errorf("completed = %v, retried = %v; want row 7 retried", completed, retried)
	}
}

// TestPutObject_Overwrite_DisplacedDeleteSucceeds_RetiresRow asserts the
// displaced copy is deleted inline and its row retired.
func TestPutObject_Overwrite_DisplacedDeleteSucceeds_RetiresRow(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	_, _ = b2.PutObject(context.Background(), "key1", bytes.NewReader([]byte("old")), 3, "", nil)

	store, c := settleStore(t, func(s *storetest.MockMetadataStore) {
		s.EXPECT().RecordObject(gomock.Any(), gomock.Any()).
			Return([]core.CleanupItem{displacedRow(3, "b2", "key1", 3)}, nil, nil).AnyTimes()
	})
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})

	if err := putSmall(mgr, "key1"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if b2.HasCopyOf("key1") {
		t.Error("displaced copy should be deleted")
	}
	completed, retried := c.outcomes()
	if len(completed) != 1 || completed[0] != 3 || len(retried) != 0 {
		t.Errorf("completed = %v, retried = %v; want row 3 retired", completed, retried)
	}
}

// TestPutObject_Overwrite_DisplacedUnknownBackend_RetiresRow asserts a row
// naming a backend no longer configured is retired without a delete.
func TestPutObject_Overwrite_DisplacedUnknownBackend_RetiresRow(t *testing.T) {
	t.Parallel()
	store, c := settleStore(t, func(s *storetest.MockMetadataStore) {
		s.EXPECT().RecordObject(gomock.Any(), gomock.Any()).
			Return([]core.CleanupItem{displacedRow(5, "gone", "key1", 300)}, nil, nil).AnyTimes()
	})
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if err := putSmall(mgr, "key1"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	completed, retried := c.outcomes()
	if len(completed) != 1 || completed[0] != 5 || len(retried) != 0 {
		t.Errorf("completed = %v, retried = %v; want row 5 retired", completed, retried)
	}
}

// TestDeleteObject_BackendDeleteFails_RetriesRow pins that a backend delete
// failure during DeleteObject sends the queued row back for retry and still
// returns nil to the caller.
func TestDeleteObject_BackendDeleteFails_RetriesRow(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.SetDeleteErr(errors.New("be timeout"))

	store, c := settleStore(t, func(s *storetest.MockMetadataStore) {
		s.EXPECT().DeleteObject(gomock.Any(), "mykey").
			Return(deletedRows("mykey", "b1"), nil, nil).AnyTimes()
	})
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if err := mgr.DeleteObject(context.Background(), "mykey"); err != nil {
		t.Fatalf("DeleteObject should succeed even if be delete fails: %v", err)
	}
	completed, retried := c.outcomes()
	if len(retried) != 1 || retried[0] != 1 || len(completed) != 0 {
		t.Errorf("completed = %v, retried = %v; want row 1 retried", completed, retried)
	}
}

// TestDeleteObjects_SettlesEveryRow asserts the rows a batch delete queues
// are deleted and retired.
func TestDeleteObjects_SettlesEveryRow(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	keys := []string{"a", "b"}
	for _, k := range keys {
		_, _ = be.PutObject(context.Background(), k, bytes.NewReader([]byte("x")), 1, "", nil)
	}
	store, c := settleStore(t, func(s *storetest.MockMetadataStore) {
		s.EXPECT().DeleteObjectsBatch(gomock.Any(), keys).DoAndReturn(stubBatchDelete("b1"))
	})
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	for i, r := range mgr.DeleteObjects(context.Background(), keys) {
		if r.Err != nil {
			t.Errorf("results[%d]: %v", i, r.Err)
		}
	}
	if be.Len() != 0 {
		t.Errorf("objects left = %d, want 0", be.Len())
	}
	completed, retried := c.outcomes()
	slices.Sort(completed)
	if !slices.Equal(completed, []int64{1, 2}) || len(retried) != 0 {
		t.Errorf("completed = %v, retried = %v; want rows 1 and 2 retired", completed, retried)
	}
}

// TestPutObject_RecordFails_LeavesIntentForReaper pins the pending-row
// pattern: a record failure discards nothing, leaving the intent for the
// reaper.
func TestPutObject_RecordFails_LeavesIntentForReaper(t *testing.T) {
	t.Parallel()
	store, c := settleStore(t, func(s *storetest.MockMetadataStore) {
		s.EXPECT().RecordObject(gomock.Any(), gomock.Any()).
			Return(nil, nil, errors.New("db error")).AnyTimes()
	})
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if err := putSmall(mgr, "mykey"); err == nil {
		t.Fatal("expected error from PutObject")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.discarded != 0 {
		t.Errorf("discards = %d, want 0 (the reaper resolves the intent)", c.discarded)
	}
}

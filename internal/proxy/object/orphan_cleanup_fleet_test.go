// -------------------------------------------------------------------------------
// Orphan Bytes Tracking Tests
//
// Author: Alex Freidah
//
// Validates the orphan bytes lifecycle: enqueue increments, successful cleanup
// decrements, exhausted cleanup preserves, displaced copies on overwrite are
// enqueued with correct sizes, and replicator capacity checks account for
// orphan bytes.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// orphanCalls accumulates the per-test interactions a migrated test
// asserts on. Each field is keyed by the store method that fed it.
type orphanCalls struct {
	mu        sync.Mutex
	enqueue   []core.CleanupItem
	increment []orphanBytesEntry
}

// orphanBytesEntry is one orphan-byte adjustment: which backend was charged
// and for how many bytes.
type orphanBytesEntry struct {
	backendName string
	sizeBytes   int64
}

// stubOrphanEnqueue captures EnqueueCleanup args.
func stubOrphanEnqueue(c *orphanCalls, err error) func(context.Context, string, string, string, int64) error {
	return func(_ context.Context, backend, key, reason string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.enqueue = append(c.enqueue, core.CleanupItem{
			BackendName: backend, ObjectKey: key, Reason: reason, SizeBytes: size,
		})
		return err
	}
}

// stubOrphanIncrement captures IncrementOrphanBytes args.
func stubOrphanIncrement(c *orphanCalls, err error) func(context.Context, string, int64) error {
	return func(_ context.Context, backend string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.increment = append(c.increment, orphanBytesEntry{backendName: backend, sizeBytes: size})
		return err
	}
}

// TestPutObject_Overwrite_EnqueuesDisplacedCopiesWithSize pins the
// overwrite path: displaced copies on other backends enqueue cleanup
// rows with the right size and orphan-bytes increment.
func TestPutObject_Overwrite_EnqueuesDisplacedCopiesWithSize(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	b2.DeleteErr = errors.New("backend down")

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).Return("b1", nil).AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b2", SizeBytes: 500}}, nil).AnyTimes()
	store.EXPECT().RecordObjectAndClearPending(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b2", SizeBytes: 500}}, nil).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, nil)

	if _, err := mgr.PutObject(context.Background(), "overwritten-key", bytes.NewReader([]byte("new")), 3, "text/plain", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if len(c.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call for displaced copy, got %d", len(c.enqueue))
	}
	if c.enqueue[0].BackendName != "b2" {
		t.Errorf("expected displaced copy enqueue for b2, got %q", c.enqueue[0].BackendName)
	}
	if c.enqueue[0].Reason != "overwrite_displaced" {
		t.Errorf("expected reason=overwrite_displaced, got %q", c.enqueue[0].Reason)
	}
	if len(c.increment) != 1 {
		t.Fatalf("expected 1 IncrementOrphanBytes call, got %d", len(c.increment))
	}
	if c.increment[0].sizeBytes != 500 {
		t.Errorf("expected orphan bytes=500, got %d", c.increment[0].sizeBytes)
	}
}

// TestDeleteObject_BackendFails_EnqueuesWithSize pins that DeleteObject
// enqueues with the correct size from the metadata.
func TestDeleteObject_BackendFails_EnqueuesWithSize(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.DeleteErr = errors.New("timeout")

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b1", SizeBytes: 2048}}, nil).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if err := mgr.DeleteObject(context.Background(), "mykey"); err != nil {
		t.Fatalf("DeleteObject should succeed even if be delete fails: %v", err)
	}
	if len(c.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(c.enqueue))
	}
	if len(c.increment) != 1 {
		t.Fatalf("expected 1 IncrementOrphanBytes call, got %d", len(c.increment))
	}
	if c.increment[0].sizeBytes != 2048 {
		t.Errorf("expected orphan bytes=2048, got %d", c.increment[0].sizeBytes)
	}
}

// TestRecordObjectOrCleanup_DisplacedCopyBackendNotFound asserts that
// when the displaced copy points at a missing backend, no enqueue
// fires.
func TestRecordObjectOrCleanup_DisplacedCopyBackendNotFound(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).Return("b1", nil).AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "gone", SizeBytes: 300}}, nil).AnyTimes()
	store.EXPECT().RecordObjectAndClearPending(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "gone", SizeBytes: 300}}, nil).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1}, nil)

	if _, err := mgr.PutObject(context.Background(), "key1", bytes.NewReader([]byte("hi")), 2, "text/plain", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if len(c.enqueue) != 0 {
		t.Errorf("expected 0 enqueue calls for unknown backend, got %d", len(c.enqueue))
	}
}

// TestRecordObjectOrCleanup_DisplacedCopyDeleteSucceeds asserts no
// enqueue when the displaced delete succeeds inline.
func TestRecordObjectOrCleanup_DisplacedCopyDeleteSucceeds(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	_, _ = b2.PutObject(context.Background(), "key1", bytes.NewReader([]byte("old")), 3, "", nil)

	c := &orphanCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).Return("b1", nil).AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b2", SizeBytes: 3}}, nil).AnyTimes()
	store.EXPECT().RecordObjectAndClearPending(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b2", SizeBytes: 3}}, nil).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanEnqueue(c, nil)).AnyTimes()
	store.EXPECT().IncrementOrphanBytes(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubOrphanIncrement(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, nil)

	if _, err := mgr.PutObject(context.Background(), "key1", bytes.NewReader([]byte("new")), 3, "", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if len(c.enqueue) != 0 {
		t.Errorf("expected 0 enqueue calls (delete succeeded), got %d", len(c.enqueue))
	}
	if len(c.increment) != 0 {
		t.Errorf("expected 0 IncrementOrphanBytes calls, got %d", len(c.increment))
	}
}

// cleanupCalls records the calls a test wants to assert against. Each
// migrated test wires the relevant DoAndReturn closures to populate the
// slices on this struct, then reads them after exercising the system
// under test.
type cleanupCalls struct {
	mu      sync.Mutex
	enqueue []core.CleanupItem
	pending []core.PendingObject
}

// stubEnqueue captures EnqueueCleanup calls into c.enqueue and returns
// the supplied error so tests can drive both happy and DB-outage paths.
func stubEnqueue(c *cleanupCalls, err error) func(context.Context, string, string, string, int64) error {
	return func(_ context.Context, backend, key, reason string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.enqueue = append(c.enqueue, core.CleanupItem{
			BackendName: backend, ObjectKey: key, Reason: reason, SizeBytes: size,
		})
		return err
	}
}

// TestDeleteObject_BackendDeleteFails_EnqueuesCleanup pins that a
// be-delete failure during DeleteObject enqueues a cleanup row but
// returns nil to the caller.
func TestDeleteObject_BackendDeleteFails_EnqueuesCleanup(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()

	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
		Return([]core.DeletedCopy{{BackendName: "b1", SizeBytes: 100}}, nil).
		AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubEnqueue(calls, nil)).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	be.SetDeleteErr(errors.New("be timeout"))

	if err := mgr.DeleteObject(context.Background(), "mykey"); err != nil {
		t.Fatalf("DeleteObject should succeed even if be delete fails: %v", err)
	}

	if len(calls.enqueue) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(calls.enqueue))
	}
	c := calls.enqueue[0]
	if c.BackendName != "b1" || c.ObjectKey != "mykey" || c.Reason != "delete_failed" {
		t.Errorf("unexpected enqueue call: %+v", c)
	}
}

// TestPutObject_RecordFails_DoesNotEnqueueOrphanCleanup pins the
// pending-row pattern: a record failure produces no cleanup_queue
// rows, only a pending intent.
func TestPutObject_RecordFails_DoesNotEnqueueOrphanCleanup(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()

	calls := &cleanupCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b1", nil).
		AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	store.EXPECT().RecordObjectAndClearPending(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("db error")).
		AnyTimes()
	store.EXPECT().InsertPending(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.PendingObject) error {
			calls.mu.Lock()
			defer calls.mu.Unlock()
			calls.pending = append(calls.pending, *p)
			return nil
		}).
		AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubEnqueue(calls, nil)).
		AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.PutObject(context.Background(), "mykey", bytes.NewReader([]byte("data")), 4, "text/plain", nil); err == nil {
		t.Fatal("expected error from PutObject")
	}

	if len(calls.enqueue) != 0 {
		t.Fatalf("expected 0 enqueue calls (pending pattern handles recovery), got %d", len(calls.enqueue))
	}
	if len(calls.pending) != 1 {
		t.Fatalf("expected 1 InsertPending call, got %d", len(calls.pending))
	}
}

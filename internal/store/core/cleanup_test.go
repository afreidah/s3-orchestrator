// -------------------------------------------------------------------------------
// Core Cleanup Orchestration Tests
//
// Author: Alex Freidah
//
// Engine-agnostic tests for the multi-step cleanup_queue / cleanup_dlq flows
// that live in core/cleanup.go. The stub TxAdapter records every transactional
// call so each test can assert the precise sequence and ensure quota state is
// (or is not) touched as the design intends. The single-tx Runner used here is
// the same canonical helper in core/runner_test.go - operations either commit
// fully or surface the error to the caller.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// CLEANUP TX STUB
// -------------------------------------------------------------------------

// cleanupTxStub is a minimal TxAdapter that records the cleanup-table
// operations relevant to MoveCleanupToDLQ. Every other TxAdapter method
// is implemented as a no-op so the stub satisfies the parent interface
// without a per-test fixture for unrelated tables.
type cleanupTxStub struct {
	getRow             CleanupQueueRow
	getRowErr          error
	insertedDLQ        []CleanupQueueRow
	insertDLQErr       error
	deletedID          int64
	deleteErr          error
	decrementOrphans   []int64
	sumDeleteRowCount  int64
	sumDeleteTotalSize int64
	sumDeleteErr       error
}

// GetCleanupQueueRow is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) GetCleanupQueueRow(_ context.Context, _ int64) (CleanupQueueRow, error) {
	return t.getRow, t.getRowErr
}

// InsertCleanupDLQ is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) InsertCleanupDLQ(_ context.Context, row *CleanupQueueRow) error {
	if t.insertDLQErr != nil {
		return t.insertDLQErr
	}
	t.insertedDLQ = append(t.insertedDLQ, *row)
	return nil
}

// HasPendingCleanup reports no outstanding delete on cleanupTxStub; the
// suppression behaviour it guards has its own fixtures in objects_test.go.
func (*cleanupTxStub) HasPendingCleanup(context.Context, string, string) (bool, error) {
	return false, nil
}

// DeleteCleanupItem is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) DeleteCleanupItem(_ context.Context, id int64) error {
	if t.deleteErr != nil {
		return t.deleteErr
	}
	t.deletedID = id
	return nil
}

// SumAndDeleteCleanupQueueRows is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) SumAndDeleteCleanupQueueRows(_ context.Context, _, _ string) (int64, int64, error) {
	return t.sumDeleteRowCount, t.sumDeleteTotalSize, t.sumDeleteErr
}

// IncrementBackendQuota is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) IncrementBackendQuota(context.Context, string, int64) error { return nil }

// DecrementBackendQuota is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) DecrementBackendQuota(context.Context, string, int64) error { return nil }

// AllBackendBytesUsed is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) AllBackendBytesUsed(context.Context) (map[string]int64, error) {
	return nil, nil
}

// SumObjectSizesByBackend is a no-op stub on cleanupTxStub.
func (t *cleanupTxStub) SumObjectSizesByBackend(context.Context) (map[string]int64, error) {
	return nil, nil
}

// SetBackendBytesUsed is a no-op stub on cleanupTxStub.
func (t *cleanupTxStub) SetBackendBytesUsed(context.Context, string, int64) error { return nil }

// DecrementOrphanBytes is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) DecrementOrphanBytes(_ context.Context, _ string, delta int64) error {
	t.decrementOrphans = append(t.decrementOrphans, delta)
	return nil
}

// AcquireKeyLock is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) AcquireKeyLock(context.Context, string) error { return nil }

// ClaimPending is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) ClaimPending(context.Context, string) (bool, error) { return false, nil }

// DeletePending is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) DeletePending(context.Context, string) error { return nil }

// GetExistingCopiesForUpdate is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) GetExistingCopiesForUpdate(context.Context, string) ([]ExistingCopy, error) {
	return nil, nil
}

// InsertObjectLocation is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) InsertObjectLocation(context.Context, *ObjectLocation) error { return nil }

// DeleteObjectCopies is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) DeleteObjectCopies(context.Context, string) error { return nil }

// GetCopiesForKeysForUpdate is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) GetCopiesForKeysForUpdate(context.Context, []string) ([]KeyedExistingCopy, error) {
	return nil, nil
}

// DeleteObjectsByKeys is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) DeleteObjectsByKeys(context.Context, []string) error { return nil }

// CheckObjectExistsOnBackend is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) CheckObjectExistsOnBackend(context.Context, string, string) (bool, error) {
	return false, nil
}

// LockObjectOnBackend is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) LockObjectOnBackend(context.Context, string, string) (*ObjectLocation, bool, error) {
	return nil, false, nil
}

// DeleteObjectFromBackend is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) DeleteObjectFromBackend(context.Context, string, string) error { return nil }

// RecordCompressionProbe is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) RecordCompressionProbe(context.Context, *CompressionProbe) error {
	return nil
}

// InsertObjectLocationIfNotExists is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) InsertObjectLocationIfNotExists(context.Context, *ObjectLocation) (bool, error) {
	return false, nil
}

// InsertReplicaConditional is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
func (t *cleanupTxStub) InsertReplicaConditional(context.Context, string, string, string) (int64, bool, error) {
	return 0, false, nil
}

// InsertObjectTag is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; the cleanup paths do not touch tags.
func (*cleanupTxStub) InsertObjectTag(context.Context, string, string, string) error { return nil }

// DeleteObjectTags is a no-op stub on cleanupTxStub.
func (*cleanupTxStub) DeleteObjectTags(context.Context, string) error { return nil }

// DeleteObjectTagsForKeys is a no-op stub on cleanupTxStub.
func (*cleanupTxStub) DeleteObjectTagsForKeys(context.Context, []string) error { return nil }

// stubRunner runs the supplied closure synchronously against the
// embedded TxAdapter so cleanup_test.go can exercise the tx body
// without is a no-op stub on cleanupTxStub so the type satisfies the
// full TxAdapter interface; only the cleanup-touching methods carry
// real test fixtures.
type stubRunner struct {
	tx TxAdapter
}

// WithTx satisfies core.Runner.
func (r *stubRunner) WithTx(ctx context.Context, fn func(ctx context.Context, tx TxAdapter) error) error {
	return fn(ctx, r.tx)
}

// runWithCleanup executes fn against a stubRunner wrapping stub. Any
// error fn returns is bubbled to t.Fatalf.
func runWithCleanup(t *testing.T, stub TxAdapter, fn func(Runner) error) {
	t.Helper()
	if err := fn(&stubRunner{tx: stub}); err != nil {
		t.Fatalf("runWithCleanup: %v", err)
	}
}

// -------------------------------------------------------------------------
// MOVE CLEANUP TO DLQ
// -------------------------------------------------------------------------

// TestMoveCleanupToDLQ_HappyPath asserts the move-to-DLQ flow performs
// exactly three transactional steps - read, insert into DLQ, delete the
// queue row - and never touches orphan_bytes. The orphan-bytes
// invariant is the load-bearing piece of #651: the bytes really are
// still on the backend, so decrementing here would lie about reclaimed
// capacity.
func TestMoveCleanupToDLQ_HappyPath(t *testing.T) {
	t.Parallel()
	stub := &cleanupTxStub{
		getRow: CleanupQueueRow{
			ID:          42,
			BackendName: "b1",
			ObjectKey:   "stale.bin",
			Reason:      "delete_failed",
			SizeBytes:   2048,
			Attempts:    10,
			CreatedAt:   time.Now().Add(-2 * time.Hour),
			LastError:   "earlier error",
		},
	}
	runWithCleanup(t, stub, func(r Runner) error {
		moved, err := MoveCleanupToDLQ(context.Background(), r, 42, "permanent failure")
		if err != nil {
			t.Fatalf("MoveCleanupToDLQ: %v", err)
		}
		if !moved {
			t.Errorf("expected moved=true")
		}
		return nil
	})

	if len(stub.insertedDLQ) != 1 {
		t.Fatalf("expected one DLQ insert, got %d", len(stub.insertedDLQ))
	}
	got := stub.insertedDLQ[0]
	if got.ID != 42 {
		t.Errorf("DLQ row.ID=%d, want 42", got.ID)
	}
	if got.LastError != "permanent failure" {
		t.Errorf("DLQ row.LastError=%q, want %q", got.LastError, "permanent failure")
	}
	if stub.deletedID != 42 {
		t.Errorf("expected DeleteCleanupItem(42), got id=%d", stub.deletedID)
	}
	if len(stub.decrementOrphans) != 0 {
		t.Errorf("orphan_bytes must NOT be decremented on DLQ move; got %v", stub.decrementOrphans)
	}
}

// TestMoveCleanupToDLQ_AlreadyGone asserts that moving a row that no
// longer exists is a benign no-op - returns (false, nil) without an
// insert or delete - so a concurrent finaliser race never corrupts the
// queue.
func TestMoveCleanupToDLQ_AlreadyGone(t *testing.T) {
	t.Parallel()
	stub := &cleanupTxStub{getRowErr: ErrCleanupItemNotFound}
	runWithCleanup(t, stub, func(r Runner) error {
		moved, err := MoveCleanupToDLQ(context.Background(), r, 1, "")
		if err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
		if moved {
			t.Errorf("expected moved=false when row missing")
		}
		return nil
	})
	if len(stub.insertedDLQ) != 0 {
		t.Errorf("DLQ insert must not happen when row missing")
	}
}

// TestMoveCleanupToDLQ_PreservesOriginalLastErrorWhenNoneSupplied
// asserts that a caller passing an empty last_error keeps the queue
// row's existing last_error so DLQ entries carry the most useful
// failure context, not an empty string.
func TestMoveCleanupToDLQ_PreservesOriginalLastErrorWhenNoneSupplied(t *testing.T) {
	t.Parallel()
	stub := &cleanupTxStub{
		getRow: CleanupQueueRow{
			ID:        7,
			LastError: "boom",
		},
	}
	runWithCleanup(t, stub, func(r Runner) error {
		_, err := MoveCleanupToDLQ(context.Background(), r, 7, "")
		return err
	})
	if len(stub.insertedDLQ) != 1 || stub.insertedDLQ[0].LastError != "boom" {
		t.Errorf("DLQ row must preserve original LastError; got %#v", stub.insertedDLQ)
	}
}

// TestMoveCleanupToDLQ_InsertFailureDoesNotDeleteRow asserts that if
// the DLQ insert fails the queue row is NOT deleted - so the next
// retry tick still finds the row to graduate. Without this guarantee a
// transient insert failure would silently drop the orphan record.
func TestMoveCleanupToDLQ_InsertFailureDoesNotDeleteRow(t *testing.T) {
	t.Parallel()
	stub := &cleanupTxStub{
		getRow:       CleanupQueueRow{ID: 99},
		insertDLQErr: errors.New("boom"),
	}
	runner := &stubRunner{tx: stub}
	moved, err := MoveCleanupToDLQ(context.Background(), runner, 99, "")
	if err == nil {
		t.Fatalf("expected error from insert failure")
	}
	if moved {
		t.Errorf("expected moved=false on insert failure")
	}
	if stub.deletedID != 0 {
		t.Errorf("DeleteCleanupItem must not run on insert failure; got id=%d", stub.deletedID)
	}
}

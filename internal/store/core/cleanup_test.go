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
	"slices"
	"strings"
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
	noopTxAdapter

	getRow       CleanupQueueRow
	getRowErr    error
	insertedDLQ  []CleanupQueueRow
	insertDLQErr error
	deletedID    int64
	deleteErr    error
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

// queueRecorder wraps a TxAdapter and records every cleanup row the
// transaction queues, numbering them from 1, or fails the insert when err is
// set.
type queueRecorder struct {
	TxAdapter

	queued []CleanupRequest
	err    error
}

// InsertCleanup records the request and hands back its position as the id,
// or fails with the seeded error.
func (q *queueRecorder) InsertCleanup(_ context.Context, c *CleanupRequest) (int64, error) {
	if q.err != nil {
		return 0, q.err
	}
	q.queued = append(q.queued, *c)
	return int64(len(q.queued)), nil
}

// stubRunner runs the supplied closure synchronously against the TxAdapter it
// holds, so a test exercises the transaction body without a database.
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

// TestMoveCleanupToDLQ_HappyPath asserts the move reads the row, inserts it
// into the DLQ and deletes the queue row.
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

// TestMoveCleanupToDLQ_SurfacesErrors verifies a failed row read or queue
// delete is reported rather than read as a row already gone.
func TestMoveCleanupToDLQ_SurfacesErrors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	for name, stub := range map[string]*cleanupTxStub{
		"read":   {getRowErr: sentinel},
		"delete": {getRow: CleanupQueueRow{ID: 3}, deleteErr: sentinel},
	} {
		moved, err := MoveCleanupToDLQ(context.Background(), &stubRunner{tx: stub}, 3, "")
		if !errors.Is(err, sentinel) || moved {
			t.Errorf("%s: moved=%v err=%v, want the failure", name, moved, err)
		}
	}
}

// -------------------------------------------------------------------------
// ENQUEUE REMOVED
// -------------------------------------------------------------------------

// TestEnqueueRemoved_QueuesEachCopy verifies every removed copy becomes one
// queued row carrying the id the insert returned, its own reason when it has
// one and the transaction's otherwise.
func TestEnqueueRemoved_QueuesEachCopy(t *testing.T) {
	t.Parallel()
	queuer := &queueRecorder{TxAdapter: &noopTxAdapter{}}
	items, err := enqueueRemoved(context.Background(), queuer, "k", CleanupReasonDeleted, []removedCopy{
		{BackendName: "b1", StorageKey: "k!a", SizeBytes: 10},
		{BackendName: "b2", StorageKey: "k!b", SizeBytes: 20, Reason: CleanupReasonSupersededIntent},
	})
	if err != nil {
		t.Fatalf("enqueueRemoved: %v", err)
	}
	want := []CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "k", StorageKey: "k!a", Reason: CleanupReasonDeleted, SizeBytes: 10},
		{ID: 2, BackendName: "b2", ObjectKey: "k", StorageKey: "k!b", Reason: CleanupReasonSupersededIntent, SizeBytes: 20},
	}
	if !slices.Equal(items, want) {
		t.Errorf("items = %+v, want %+v", items, want)
	}
	if len(queuer.queued) != 2 {
		t.Errorf("queued %d rows, want 2", len(queuer.queued))
	}
}

// TestEnqueueRemoved_NothingRemoved verifies an empty set queues nothing and
// returns nil.
func TestEnqueueRemoved_NothingRemoved(t *testing.T) {
	t.Parallel()
	queuer := &queueRecorder{TxAdapter: &noopTxAdapter{}}
	items, err := enqueueRemoved(context.Background(), queuer, "k", CleanupReasonDeleted, nil)
	if err != nil || items != nil || len(queuer.queued) != 0 {
		t.Errorf("items=%+v err=%v queued=%v, want nothing", items, err, queuer.queued)
	}
}

// TestEnqueueRemoved_InsertFailure verifies a row that cannot be queued fails
// the call, naming the backend, so the transaction rolls back rather than
// committing a removal whose bytes nothing names.
func TestEnqueueRemoved_InsertFailure(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("insert failed")
	queuer := &queueRecorder{TxAdapter: &noopTxAdapter{}, err: sentinel}
	items, err := enqueueRemoved(context.Background(), queuer, "k", CleanupReasonDeleted, []removedCopy{{BackendName: "b1"}})
	if !errors.Is(err, sentinel) || items != nil {
		t.Fatalf("items=%+v err=%v, want the insert failure", items, err)
	}
	if !strings.Contains(err.Error(), "b1") {
		t.Errorf("err = %v, want it to name the backend", err)
	}
}

// -------------------------------------------------------------------------
// DISCARD PENDING
// -------------------------------------------------------------------------

// discardTxStub drives DiscardPending: it decides whether the intent is still
// held and records the intents it deletes.
type discardTxStub struct {
	noopTxAdapter

	lost      bool
	claimErr  error
	lockErr   error
	deleteErr error
	deleted   []string
}

func (s *discardTxStub) AcquireKeyLock(context.Context, string) error { return s.lockErr }

func (s *discardTxStub) ClaimPending(context.Context, string) (bool, error) {
	return !s.lost, s.claimErr
}

func (s *discardTxStub) DeletePending(_ context.Context, intentID string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, intentID)
	return nil
}

// discardedIntent is the upload a write gave up on after its bytes landed
// on b1.
func discardedIntent() *PendingObject {
	return &PendingObject{IntentID: "i-9", ObjectKey: "k", BackendName: "b1", StorageKey: "k!i-9", SizeBytes: 42}
}

// TestDiscardPending_DropsAndQueues verifies a held intent is deleted and its
// bytes queued under the caller's reason.
func TestDiscardPending_DropsAndQueues(t *testing.T) {
	t.Parallel()
	stub := &discardTxStub{}
	cleanup, err := DiscardPending(context.Background(), &stubRunner{tx: stub}, discardedIntent(), "put_failed")
	if err != nil {
		t.Fatalf("DiscardPending: %v", err)
	}
	if !slices.Equal(stub.deleted, []string{"i-9"}) {
		t.Errorf("deleted = %v, want the discarded intent", stub.deleted)
	}
	want := []CleanupItem{{BackendName: "b1", ObjectKey: "k", StorageKey: "k!i-9", Reason: "put_failed", SizeBytes: 42}}
	if !slices.Equal(cleanup, want) {
		t.Errorf("cleanup = %+v, want %+v", cleanup, want)
	}
}

// TestDiscardPending_IntentAlreadyGone verifies an intent someone else already
// resolved queues nothing, since whoever removed it queued its bytes.
func TestDiscardPending_IntentAlreadyGone(t *testing.T) {
	t.Parallel()
	stub := &discardTxStub{lost: true}
	cleanup, err := DiscardPending(context.Background(), &stubRunner{tx: stub}, discardedIntent(), "put_failed")
	if err != nil || cleanup != nil || len(stub.deleted) != 0 {
		t.Errorf("cleanup=%+v err=%v deleted=%v, want nothing", cleanup, err, stub.deleted)
	}
}

// TestDiscardPending_FailuresRollBack verifies each step's failure surfaces
// with nothing queued.
func TestDiscardPending_FailuresRollBack(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("step failed")
	cases := map[string]TxAdapter{
		"key lock": &discardTxStub{lockErr: sentinel},
		"claim":    &discardTxStub{claimErr: sentinel},
		"delete":   &discardTxStub{deleteErr: sentinel},
		"queue":    &queueRecorder{TxAdapter: &discardTxStub{}, err: sentinel},
	}
	for name, tx := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cleanup, err := DiscardPending(context.Background(), &stubRunner{tx: tx}, discardedIntent(), "put_failed")
			if !errors.Is(err, sentinel) || cleanup != nil {
				t.Errorf("cleanup=%+v err=%v, want the %s failure", cleanup, err, name)
			}
		})
	}
}

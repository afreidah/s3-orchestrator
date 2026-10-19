// -------------------------------------------------------------------------------
// Core Cleanup Orchestration Tests
//
// Author: Alex Freidah
//
// Engine-agnostic tests for the cleanup_queue flows that live in
// core/cleanup.go. The stub TxAdapter records every transactional call so each
// test can assert the precise sequence. The single-tx Runner used here runs
// the transaction body directly, so operations either commit fully or surface
// the error to the caller.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// CLEANUP TX STUB
// -------------------------------------------------------------------------

// queueRecorder wraps a TxAdapter and records every cleanup row the
// transaction queues, numbering them from 1 and stamping claim on each, or
// fails the insert when err is set.
type queueRecorder struct {
	TxAdapter

	queued []CleanupRequest
	claim  *CleanupClaim
	err    error
}

// InsertCleanup records the request and hands back the queued row with its
// position as the id and the recorder's claim, or fails with the seeded error.
func (q *queueRecorder) InsertCleanup(_ context.Context, c *CleanupRequest) (CleanupItem, error) {
	if q.err != nil {
		return CleanupItem{}, q.err
	}
	q.queued = append(q.queued, *c)
	return CleanupItem{
		ID:          int64(len(q.queued)),
		BackendName: c.BackendName,
		ObjectKey:   c.ObjectKey,
		StorageKey:  c.StorageKey,
		Reason:      c.Reason,
		SizeBytes:   c.SizeBytes,
		Claim:       q.claim,
	}, nil
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

// -------------------------------------------------------------------------
// ENQUEUE REMOVED
// -------------------------------------------------------------------------

// TestEnqueueRemoved_QueuesEachCopy verifies every removed copy becomes one
// queued row exactly as the insert returned it, claim included, with its own
// reason when it has one and the transaction's otherwise.
func TestEnqueueRemoved_QueuesEachCopy(t *testing.T) {
	t.Parallel()
	claim := &CleanupClaim{At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), By: "inst-1"}
	queuer := &queueRecorder{TxAdapter: &noopTxAdapter{}, claim: claim}
	items, err := enqueueRemoved(context.Background(), queuer, "k", CleanupReasonDeleted, []removedCopy{
		{BackendName: "b1", StorageKey: "k!a", SizeBytes: 10},
		{BackendName: "b2", StorageKey: "k!b", SizeBytes: 20, Reason: CleanupReasonSupersededIntent},
	})
	if err != nil {
		t.Fatalf("enqueueRemoved: %v", err)
	}
	want := []CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "k", StorageKey: "k!a", Reason: CleanupReasonDeleted, SizeBytes: 10, Claim: claim},
		{ID: 2, BackendName: "b2", ObjectKey: "k", StorageKey: "k!b", Reason: CleanupReasonSupersededIntent, SizeBytes: 20, Claim: claim},
	}
	if !reflect.DeepEqual(items, want) {
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

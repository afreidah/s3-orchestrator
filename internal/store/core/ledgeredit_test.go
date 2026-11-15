// -------------------------------------------------------------------------------
// Ledger Edit Tests
//
// Author: Alex Freidah
//
// Engine-agnostic tests for what a ledger edit writes when its transaction
// commits: the cleanup rows its changes queued, in one statement, and then the
// stripe charges they made, in another. The stub runner runs each transaction
// body directly, so an operation either commits fully or surfaces its error.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// STUBS
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

// InsertCleanup records the requests and hands back the queued rows with their
// positions as the ids and the recorder's claim, or fails with the seeded error.
func (q *queueRecorder) InsertCleanup(_ context.Context, requests ...*CleanupRequest) ([]CleanupItem, error) {
	if q.err != nil {
		return nil, q.err
	}
	items := make([]CleanupItem, len(requests))
	for i, c := range requests {
		q.queued = append(q.queued, *c)
		items[i] = CleanupItem{CleanupRequest: *c, ID: int64(len(q.queued)), Claim: q.claim}
	}
	return items, nil
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

// stubOps binds the shared transactional methods to a stub runner over tx, so
// a test drives them the way an engine store does.
func stubOps(tx TxAdapter) TxOps {
	return NewTxOps(&stubRunner{tx: tx}, 1)
}

// stripeRecordingTxStub captures each stripe charge statement: the rows it
// wrote, their signed deltas and what they added up to.
type stripeRecordingTxStub struct {
	noopTxAdapter
	calls  int
	rows   []stripeRow
	deltas []int64
	total  int64
}

func (s *stripeRecordingTxStub) AdjustQuotaStripes(_ context.Context, charges ...StripeCharge) error {
	s.calls++
	for _, c := range charges {
		s.rows = append(s.rows, stripeRow{c.Backend, c.Stripe})
		s.deltas = append(s.deltas, c.Delta)
		s.total += c.Delta
	}
	return nil
}

// -------------------------------------------------------------------------
// CLEANUP ROWS
// -------------------------------------------------------------------------

// TestLedgerEdit_CommitQueuesEachRow verifies every queued deletion becomes one
// row exactly as the insert returned it, claim included, and that bytes with no
// path of their own are queued at their key.
func TestLedgerEdit_CommitQueuesEachRow(t *testing.T) {
	t.Parallel()
	claim := &CleanupClaim{At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), By: "inst-1"}
	queuer := &queueRecorder{TxAdapter: &noopTxAdapter{}, claim: claim}
	e := newLedgerEdit(queuer)
	e.queue("k", CleanupReasonDeleted, "b1", "k!a", 10)
	e.queue("k", CleanupReasonSupersededIntent, "b2", "", 20)
	items, err := e.commit(context.Background())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	want := []CleanupItem{
		{ID: 1, BackendName: "b1", ObjectKey: "k", StorageKey: "k!a", Reason: CleanupReasonDeleted, SizeBytes: 10, Claim: claim},
		{ID: 2, BackendName: "b2", ObjectKey: "k", StorageKey: "k", Reason: CleanupReasonSupersededIntent, SizeBytes: 20, Claim: claim},
	}
	if !reflect.DeepEqual(items, want) {
		t.Errorf("items = %+v, want %+v", items, want)
	}
}

// TestLedgerEdit_RemovedCopiesQueueTheirOwnPaths verifies every removed copy is
// queued at its own path, one on a backend the new write also lands on
// included, since the new write stores its bytes at a path of its own.
func TestLedgerEdit_RemovedCopiesQueueTheirOwnPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tx := &stripeRecordingTxStub{}
	e := newLedgerEdit(tx)
	old := []ExistingCopy{
		{ObjectKey: "k", BackendName: "b1", StorageKey: "k!old", SizeBytes: 100},
		{ObjectKey: "k", BackendName: "b2", StorageKey: "k!b", SizeBytes: 200},
	}
	if err := e.removeCopies(ctx, CleanupReasonOverwritten, []string{"k"}, old); err != nil {
		t.Fatalf("removeCopies: %v", err)
	}
	if err := e.insertCopy(ctx, &ObjectLocation{ObjectKey: "k", BackendName: "b1", StorageKey: "k!new", SizeBytes: 100}); err != nil {
		t.Fatalf("insertCopy: %v", err)
	}
	items, err := e.commit(ctx)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	var paths []string
	for _, item := range items {
		paths = append(paths, item.StorageKey)
	}
	if !slices.Equal(paths, []string{"k!old", "k!b"}) {
		t.Errorf("queued %v, want both old copies at their own paths", paths)
	}
	if !slices.Equal(tx.deltas, []int64{-200}) {
		t.Errorf("deltas = %v, want b1 cancelled out and b2 debited", tx.deltas)
	}
}

// -------------------------------------------------------------------------
// STRIPE CHARGES
// -------------------------------------------------------------------------

// TestLedgerEdit_ChargesEachKeyOnItsStripeInOrder verifies one commit writes
// every changed row in one statement, ordered by backend, each with its sign
// intact and on the stripe its key selects, so a later credit cancels it.
func TestLedgerEdit_ChargesEachKeyOnItsStripeInOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tx := &stripeRecordingTxStub{}
	e := newLedgerEdit(tx)
	for _, loc := range []*ObjectLocation{
		{ObjectKey: "bucket/key", BackendName: "b3", SizeBytes: 100},
		{ObjectKey: "bucket/key", BackendName: "b1", SizeBytes: 50},
	} {
		if err := e.insertCopy(ctx, loc); err != nil {
			t.Fatalf("insertCopy: %v", err)
		}
	}
	e.debit(&ExistingCopy{ObjectKey: "bucket/key", BackendName: "b2", SizeBytes: 75})
	if _, err := e.commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	stripe := StripeFor("bucket/key")
	want := []stripeRow{{"b1", stripe}, {"b2", stripe}, {"b3", stripe}}
	if tx.calls != 1 || !slices.Equal(tx.rows, want) || !slices.Equal(tx.deltas, []int64{50, -75, 100}) {
		t.Errorf("calls=%d rows=%v deltas=%v, want one statement over %v with deltas [50 -75 100]", tx.calls, tx.rows, tx.deltas, want)
	}
}

// TestLedgerEdit_NothingChangedWritesNothing verifies an edit that changed
// nothing, or whose credit and debit cancelled out, issues no statement and
// reports nothing queued.
func TestLedgerEdit_NothingChangedWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tx := &stripeRecordingTxStub{}
	if items, err := newLedgerEdit(tx).commit(ctx); err != nil || items != nil {
		t.Errorf("empty edit: items=%+v err=%v, want nothing", items, err)
	}
	e := newLedgerEdit(tx)
	if err := e.insertCopy(ctx, &ObjectLocation{ObjectKey: "k", BackendName: "b1", SizeBytes: 5}); err != nil {
		t.Fatalf("insertCopy: %v", err)
	}
	e.debit(&ExistingCopy{ObjectKey: "k", BackendName: "b1", SizeBytes: 5})
	if _, err := e.commit(ctx); err != nil || tx.calls != 0 {
		t.Errorf("cancelled edit: calls=%d err=%v, want no statement", tx.calls, err)
	}
}

// TestLedgerEdit_FailedWriteFailsCommit verifies a failed cleanup insert or
// stripe charge fails the commit with nothing reported queued, which rolls the
// transaction back rather than committing rows whose bytes nothing names.
func TestLedgerEdit_FailedWriteFailsCommit(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("write failed")
	cases := map[string]TxAdapter{
		"queue":  &queueRecorder{TxAdapter: &noopTxAdapter{}, err: sentinel},
		"charge": &quotaTxStub{adjustErr: sentinel},
	}
	for name, tx := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			e := newLedgerEdit(tx)
			e.queue("k", CleanupReasonDeleted, "b1", "k!a", 10)
			e.debit(&ExistingCopy{ObjectKey: "k", BackendName: "b1", SizeBytes: 10})
			if items, err := e.commit(ctx); !errors.Is(err, sentinel) || items != nil {
				t.Errorf("items=%+v err=%v, want the %s failure", items, err, name)
			}
		})
	}
}

// TestStripeDeltas_ChargeWritesEachRowOnceInOrder spreads keys across every
// stripe of two backends and asserts each row is written once, in one global
// order, carrying the whole batch's bytes. That order is what stops two batches
// holding the same rows from deadlocking.
func TestStripeDeltas_ChargeWritesEachRowOnceInOrder(t *testing.T) {
	t.Parallel()
	const keys = 1000
	deltas := stripeDeltas{}
	for i := range keys {
		key := "bucket/key-" + strconv.Itoa(i)
		deltas.add(key, "b", -1)
		deltas.add(key, "a", -1)
	}

	tx := &stripeRecordingTxStub{}
	if err := deltas.charge(context.Background(), tx); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if len(tx.rows) != 2*QuotaStripeCount {
		t.Fatalf("wrote %d rows, want %d: one per stripe of each backend", len(tx.rows), 2*QuotaStripeCount)
	}
	for i := 1; i < len(tx.rows); i++ {
		prev, cur := tx.rows[i-1], tx.rows[i]
		if prev.backend > cur.backend || (prev.backend == cur.backend && prev.stripe >= cur.stripe) {
			t.Errorf("row %d out of order: %+v after %+v", i, cur, prev)
		}
	}
	if tx.total != -2*keys {
		t.Errorf("total = %d, want %d", tx.total, -2*keys)
	}
}

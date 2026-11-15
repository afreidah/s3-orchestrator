// -------------------------------------------------------------------------------
// Statement Count Tests
//
// Author: Alex Freidah
//
// A transaction's cost is mostly its round trips to the database, so the steps
// that touch many rows must each stay one statement however many rows there are.
// The stripe charge locks rows every write to those stripes waits on, so it is
// a transaction's last statement.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

// statementCountTx counts the calls a transaction makes for the steps that touch
// many rows, over the copies and parts it is seeded with, and records the order
// of every write. lost makes every intent claim fail, as for an overtaken write.
type statementCountTx struct {
	noopTxAdapter

	copies   []ExistingCopy
	parts    []MultipartPart
	replaced *MultipartPart
	lost     bool
	inserts  int
	clears   int
	charges  int
	order    []string
}

// The writes statementCountTx records the order of.
const (
	stmtQueue         = "queue cleanup"
	stmtClear         = "clear intents"
	stmtCharge        = "charge stripes"
	stmtDeletePending = "delete intent"
	stmtWriteCopy     = "write copy"
	stmtWriteTags     = "write tags"
	stmtWritePart     = "write part"
)

func (s *statementCountTx) GetExistingCopiesForUpdate(context.Context, ...string) ([]ExistingCopy, error) {
	return s.copies, nil
}

func (s *statementCountTx) ClaimPending(context.Context, string) (bool, error) {
	return !s.lost, nil
}

// LockObjectOnBackend returns the seeded copy on backend, as the locked source
// of a placement.
func (s *statementCountTx) LockObjectOnBackend(_ context.Context, key, backend string) (*ObjectLocation, bool, error) {
	c, ok := copyOnBackend(s.copies, backend)
	if !ok {
		return nil, false, nil
	}
	return &ObjectLocation{ObjectKey: key, BackendName: backend, StorageKey: c.StorageKey, SizeBytes: c.SizeBytes}, true, nil
}

func (s *statementCountTx) InsertObjectLocation(context.Context, *ObjectLocation) error {
	s.order = append(s.order, stmtWriteCopy)
	return nil
}

func (s *statementCountTx) InsertObjectLocationIfNotExists(context.Context, *ObjectLocation) (bool, error) {
	s.order = append(s.order, stmtWriteCopy)
	return true, nil
}

func (s *statementCountTx) DeleteObjectCopies(context.Context, ...string) error {
	s.order = append(s.order, stmtWriteCopy)
	return nil
}

func (s *statementCountTx) DeleteObjectFromBackend(context.Context, string, string) error {
	s.order = append(s.order, stmtWriteCopy)
	return nil
}

func (s *statementCountTx) DeleteObjectTags(context.Context, ...string) error {
	s.order = append(s.order, stmtWriteTags)
	return nil
}

func (s *statementCountTx) UpsertPart(context.Context, *RecordPartParams) (*MultipartPart, error) {
	s.order = append(s.order, stmtWritePart)
	return s.replaced, nil
}

func (s *statementCountTx) LockMultipartUpload(context.Context, string, UploadLock) (string, time.Time, error) {
	return partBackend, time.Time{}, nil
}

func (s *statementCountTx) DeleteMultipartUpload(context.Context, string) ([]MultipartPart, error) {
	return s.parts, nil
}

func (s *statementCountTx) InsertCleanup(ctx context.Context, requests ...*CleanupRequest) ([]CleanupItem, error) {
	s.inserts++
	s.order = append(s.order, stmtQueue)
	return s.noopTxAdapter.InsertCleanup(ctx, requests...)
}

func (s *statementCountTx) ClearPendingForKey(context.Context, []string, ...string) ([]SupersededIntent, error) {
	s.clears++
	s.order = append(s.order, stmtClear)
	return nil, nil
}

func (s *statementCountTx) AdjustQuotaStripes(context.Context, ...StripeCharge) error {
	s.charges++
	s.order = append(s.order, stmtCharge)
	return nil
}

func (s *statementCountTx) DeletePending(context.Context, string) error {
	s.order = append(s.order, stmtDeletePending)
	return nil
}

// TestLedgerChanges_QueueOnceAndChargeLast verifies every transaction that
// changes the ledger queues its cleanup rows and charges its stripes in one
// statement each, the charge after every other write so its rows are held only
// until the commit.
func TestLedgerChanges_QueueOnceAndChargeLast(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	held := ExistingCopy{ObjectKey: "key", BackendName: "b1", StorageKey: "key!a", SizeBytes: 1}
	other := ExistingCopy{ObjectKey: "key", BackendName: "b2", StorageKey: "key!b", SizeBytes: 1}
	source := &ObjectLocation{ObjectKey: "key", BackendName: "b1", StorageKey: "key!a", SizeBytes: 1}
	intent := func(role PendingRole) *PendingObject {
		return &PendingObject{IntentID: "i1", ObjectKey: "key", BackendName: "b2", StorageKey: "key!i1", SizeBytes: 2, Role: role, CreatedAt: time.Now()}
	}
	record := func(o TxOps) error {
		_, err := o.RecordObject(ctx, &RecordObjectRequest{Key: "key", Size: 2, Copies: []ObjectCopy{{Backend: "b2", IntentID: "i1"}}})
		return err
	}
	place := func(target PendingRole, move bool) func(TxOps) error {
		return placeRun(ctx, source, intent(target), move)
	}
	cases := []struct {
		name string
		stub *statementCountTx
		run  func(TxOps) error
	}{
		{"record a new key", &statementCountTx{}, record},
		{"overwrite", &statementCountTx{copies: []ExistingCopy{held}}, record},
		{"overtaken write", &statementCountTx{lost: true}, record},
		{"delete", &statementCountTx{copies: []ExistingCopy{held, other}}, func(o TxOps) error {
			_, err := o.DeleteObject(ctx, "key")
			return err
		}},
		{"delete one copy", &statementCountTx{copies: []ExistingCopy{held}}, func(o TxOps) error {
			_, err := o.DeleteObjectLocation(ctx, source, 0, "excess")
			return err
		}},
		{"replica", &statementCountTx{copies: []ExistingCopy{held}}, place(PendingRoleCompanion, false)},
		{"move", &statementCountTx{copies: []ExistingCopy{held}}, place(PendingRoleCompanion, true)},
		{"refused placement", &statementCountTx{copies: []ExistingCopy{held, other}}, place(PendingRoleCompanion, false)},
		{"promote", &statementCountTx{copies: []ExistingCopy{held}}, func(o TxOps) error {
			_, _, err := o.PromotePending(ctx, intent(PendingRolePrimary))
			return err
		}},
		{"discard a companion", &statementCountTx{copies: []ExistingCopy{held}}, func(o TxOps) error {
			_, _, err := o.PromotePending(ctx, intent(PendingRoleCompanion))
			return err
		}},
		{"companion commit", &statementCountTx{}, func(o TxOps) error {
			_, _, err := o.CommitCompanionCopy(ctx, intent(PendingRoleCompanion))
			return err
		}},
		{"import", &statementCountTx{}, func(o TxOps) error {
			_, err := o.ImportObject(ctx, &ImportObjectRequest{Key: "key", Backend: "b1", Size: 3, Form: &StoredForm{}})
			return err
		}},
		{"discard an intent", &statementCountTx{}, func(o TxOps) error {
			_, err := o.DiscardPending(ctx, intent(PendingRolePrimary), "put_failed")
			return err
		}},
		{"replace a part", &statementCountTx{replaced: &MultipartPart{PartNumber: 1, SizeBytes: 1}}, func(o TxOps) error {
			_, err := o.RecordPart(ctx, &RecordPartParams{UploadID: partUploadID, PartNumber: 1, Intent: intent(PendingRolePrimary), CompletionStaleBefore: markCutoff})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.run(stubOps(tc.stub)); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			tc.stub.requireQueuedOnceChargedLast(t)
		})
	}
}

// placeRun places intent's copy of source, as a move when move is set. A refused
// placement still commits what it queued, so the refusal is not a failure here.
func placeRun(ctx context.Context, source *ObjectLocation, intent *PendingObject, move bool) func(TxOps) error {
	return func(o TxOps) error {
		_, _, err := o.PlaceCopy(ctx, &Placement{Source: source, Intent: intent, RemoveSource: move,
			Reasons: MoveReasons{Orphan: "orphan", SourceDelete: "source_delete"}})
		if errors.Is(err, ErrTargetRefused) {
			return nil
		}
		return err
	}
}

// requireQueuedOnceChargedLast fails unless the transaction queued its cleanup
// rows and charged its stripes in at most one statement each, the charge last.
func (s *statementCountTx) requireQueuedOnceChargedLast(t *testing.T) {
	t.Helper()
	if s.inserts > 1 || s.charges > 1 {
		t.Errorf("queued %d times and charged %d times, want at most once each: %v", s.inserts, s.charges, s.order)
	}
	if i := slices.Index(s.order, stmtCharge); i >= 0 && i != len(s.order)-1 {
		t.Errorf("writes = %v, want the stripe charge last", s.order)
	}
}

// TestDeleteObjectsBatch_StatementsDoNotGrowWithKeys verifies a batch clears its
// intents, queues its bytes and charges its stripes in one statement each.
func TestDeleteObjectsBatch_StatementsDoNotGrowWithKeys(t *testing.T) {
	t.Parallel()
	const keyCount = 100
	stub := &statementCountTx{}
	keys := make([]string, keyCount)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
		for _, backend := range []string{"b1", "b2"} {
			stub.copies = append(stub.copies, ExistingCopy{ObjectKey: keys[i], BackendName: backend, SizeBytes: 1})
		}
	}
	cleanup, err := stubOps(stub).DeleteObjectsBatch(context.Background(), keys)
	if err != nil {
		t.Fatalf("DeleteObjectsBatch: %v", err)
	}
	if len(cleanup) != len(stub.copies) {
		t.Errorf("queued %d rows, want one per copy (%d)", len(cleanup), len(stub.copies))
	}
	if stub.inserts != 1 || stub.clears != 1 || stub.charges != 1 {
		t.Errorf("inserts=%d clears=%d charges=%d, want one statement each", stub.inserts, stub.clears, stub.charges)
	}
}

// TestDeleteMultipartUpload_QueuesPartsInOneStatement verifies an upload's parts
// are queued together, since an upload can hold ten thousand of them.
func TestDeleteMultipartUpload_QueuesPartsInOneStatement(t *testing.T) {
	t.Parallel()
	const partCount = 500
	stub := &statementCountTx{parts: make([]MultipartPart, partCount)}
	for i := range stub.parts {
		stub.parts[i] = MultipartPart{PartNumber: i + 1, SizeBytes: 1}
	}
	cleanup, err := stubOps(stub).DeleteMultipartUpload(context.Background(), partUploadID, "abort", markCutoff)
	if err != nil {
		t.Fatalf("DeleteMultipartUpload: %v", err)
	}
	if len(cleanup) != partCount {
		t.Errorf("queued %d rows, want %d", len(cleanup), partCount)
	}
	if stub.inserts != 1 {
		t.Errorf("inserts = %d, want one statement", stub.inserts)
	}
}

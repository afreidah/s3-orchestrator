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
	"fmt"
	"slices"
	"testing"
	"time"
)

// statementCountTx counts the calls a transaction makes for the steps that touch
// many rows, over the copies and parts it is seeded with, and the order of the
// writes that follow its inserts.
type statementCountTx struct {
	noopTxAdapter

	copies  []ExistingCopy
	parts   []MultipartPart
	inserts int
	clears  int
	charges int
	order   []string
}

// The writes statementCountTx records the order of.
const (
	stmtQueue         = "queue cleanup"
	stmtClear         = "clear intents"
	stmtCharge        = "charge stripes"
	stmtDeletePending = "delete intent"
)

func (s *statementCountTx) GetExistingCopiesForUpdate(context.Context, ...string) ([]ExistingCopy, error) {
	return s.copies, nil
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

// TestRecordObject_ChargesStripesLast verifies an overwrite clears the key's
// intents and queues the displaced bytes before it charges the stripes.
func TestRecordObject_ChargesStripesLast(t *testing.T) {
	t.Parallel()
	stub := &statementCountTx{copies: []ExistingCopy{{ObjectKey: "key", BackendName: "b1", SizeBytes: 1}}}
	req := &RecordObjectRequest{Key: "key", Size: 2, Copies: []ObjectCopy{{Backend: "b1", IntentID: "i1"}}}
	if _, err := stubOps(stub).RecordObject(context.Background(), req); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if want := []string{stmtClear, stmtQueue, stmtCharge}; !slices.Equal(stub.order, want) {
		t.Errorf("writes = %v, want %v", stub.order, want)
	}
}

// TestCommitCompanionCopy_ChargesStripesLast verifies a companion commit deletes
// its intent before it charges the stripes.
func TestCommitCompanionCopy_ChargesStripesLast(t *testing.T) {
	t.Parallel()
	stub := &statementCountTx{}
	p := &PendingObject{IntentID: "i1", ObjectKey: "key", BackendName: "b1", StorageKey: "key!i1", SizeBytes: 2, Role: PendingRoleCompanion}
	if _, _, err := stubOps(stub).CommitCompanionCopy(context.Background(), p); err != nil {
		t.Fatalf("CommitCompanionCopy: %v", err)
	}
	if want := []string{stmtDeletePending, stmtCharge}; !slices.Equal(stub.order, want) {
		t.Errorf("writes = %v, want %v", stub.order, want)
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

// TestChargeStripes_OneStatementAcrossBackends verifies a mutation touching
// several backends charges them together.
func TestChargeStripes_OneStatementAcrossBackends(t *testing.T) {
	t.Parallel()
	stub := &statementCountTx{}
	if err := chargeStripes(context.Background(), stub, "key", quotaDeltas{"b1": -1, "b2": -2}); err != nil {
		t.Fatalf("chargeStripes: %v", err)
	}
	if stub.charges != 1 {
		t.Errorf("charges = %d, want one statement", stub.charges)
	}
}

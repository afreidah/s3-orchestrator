// -------------------------------------------------------------------------------
// Statement Count Tests
//
// Author: Alex Freidah
//
// A transaction's cost is mostly its round trips to the database, so the steps
// that touch many rows must each stay one statement however many rows there are.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// statementCountTx counts the calls a transaction makes for the steps that touch
// many rows, over the copies and parts it is seeded with.
type statementCountTx struct {
	noopTxAdapter

	copies  []ExistingCopy
	parts   []MultipartPart
	inserts int
	clears  int
	charges int
}

func (s *statementCountTx) GetExistingCopiesForUpdate(context.Context, ...string) ([]ExistingCopy, error) {
	return s.copies, nil
}

func (s *statementCountTx) LockMultipartUpload(context.Context, string) (string, time.Time, error) {
	return partBackend, time.Time{}, nil
}

func (s *statementCountTx) DeleteMultipartUpload(context.Context, string) ([]MultipartPart, error) {
	return s.parts, nil
}

func (s *statementCountTx) InsertCleanup(ctx context.Context, requests ...*CleanupRequest) ([]CleanupItem, error) {
	s.inserts++
	return s.noopTxAdapter.InsertCleanup(ctx, requests...)
}

func (s *statementCountTx) ClearPendingForKey(context.Context, []string, ...string) ([]SupersededIntent, error) {
	s.clears++
	return nil, nil
}

func (s *statementCountTx) AdjustQuotaStripes(context.Context, ...StripeCharge) error {
	s.charges++
	return nil
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

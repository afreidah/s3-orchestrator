// -------------------------------------------------------------------------------
// Core DeleteObjectLocation Tests
//
// Author: Alex Freidah
//
// Engine-agnostic coverage for DeleteObjectLocation: the row is deleted and the
// backend quota debited by the size read from the locked copy set, a copy that
// is no longer present is a benign no-op, and a quota-debit failure surfaces so
// the transaction rolls back. Reuses excessTxStub from the RemoveExcessCopy tests.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"testing"
)

// runDeleteLocation invokes DeleteObjectLocation against the stub.
func runDeleteLocation(stub *excessTxStub, key, backend string) error {
	return DeleteObjectLocation(context.Background(), &stubRunner{tx: stub}, key, backend)
}

// TestDeleteObjectLocation_DebitsLockedSize verifies the copy is deleted and the
// quota debited by the size read from the locked row, not a caller-supplied one.
func TestDeleteObjectLocation_DebitsLockedSize(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b1", SizeBytes: 100},
		{BackendName: "b2", SizeBytes: 200},
	}})
	if err := runDeleteLocation(stub, "k", "b1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.deleted) != 1 || stub.deleted[0] != "b1" {
		t.Errorf("expected b1 deleted, got %v", stub.deleted)
	}
	if len(stub.ops) != 1 || stub.ops[0].backend != "b1" || stub.ops[0].delta != -100 {
		t.Errorf("expected quota debit of 100 for b1 from locked row, got %v", stub.ops)
	}
}

// TestDeleteObjectLocation_NoOpWhenCopyGone pins the benign no-op where the
// target backend no longer holds a copy: nothing is deleted and no quota moves.
func TestDeleteObjectLocation_NoOpWhenCopyGone(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b2", SizeBytes: 200},
	}})
	if err := runDeleteLocation(stub, "k", "b1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.deleted) != 0 || len(stub.ops) != 0 {
		t.Errorf("expected no delete or debit, got deleted=%v ops=%v", stub.deleted, stub.ops)
	}
}

// TestDeleteObjectLocation_ReadError verifies a failure to read the locked copy
// set surfaces verbatim and mutates nothing.
func TestDeleteObjectLocation_ReadError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("read failed")
	stub := newExcessStub(&excessTxStub{existingErr: sentinel})
	if err := runDeleteLocation(stub, "k", "b1"); !errors.Is(err, sentinel) {
		t.Errorf("expected read error, got %v", err)
	}
	if len(stub.deleted) != 0 || len(stub.ops) != 0 {
		t.Errorf("nothing should mutate on a read error, got deleted=%v ops=%v", stub.deleted, stub.ops)
	}
}

// TestDeleteObjectLocation_QuotaError verifies a quota-debit failure surfaces so
// the transaction rolls back rather than leaving quota and ledger disagreeing.
func TestDeleteObjectLocation_QuotaError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("quota debit failed")
	stub := newExcessStub(&excessTxStub{
		quotaTxStub: &quotaTxStub{failOn: "b1", failErr: sentinel},
		existing:    []ExistingCopy{{BackendName: "b1", SizeBytes: 100}},
	})
	if err := runDeleteLocation(stub, "k", "b1"); !errors.Is(err, sentinel) {
		t.Errorf("expected quota error, got %v", err)
	}
}

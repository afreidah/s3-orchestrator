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

// batchTxStub drives DeleteObjectsBatch: it returns a seeded keyed copy set so
// the batch gets past its empty-result short circuit, and inherits the tag and
// lock hooks from the embedded quotaTxStub.
type batchTxStub struct {
	*quotaTxStub
	keyed []KeyedExistingCopy
}

// GetCopiesForKeysForUpdate returns the seeded keyed copy set.
func (s *batchTxStub) GetCopiesForKeysForUpdate(context.Context, []string) ([]KeyedExistingCopy, error) {
	return s.keyed, nil
}

// newBatchStub builds a batch stub with the embedded quota stub initialized.
func newBatchStub(s *batchTxStub) *batchTxStub {
	if s.quotaTxStub == nil {
		s.quotaTxStub = &quotaTxStub{}
	}
	return s
}

// TestDeleteObjectsBatch_ClearsTagsForEveryKey verifies the batch clears tags
// in one statement rather than a loop, and covers every key it was given.
func TestDeleteObjectsBatch_ClearsTagsForEveryKey(t *testing.T) {
	t.Parallel()
	stub := newBatchStub(&batchTxStub{keyed: []KeyedExistingCopy{
		{ObjectKey: "k1", BackendName: "b1", SizeBytes: 10},
		{ObjectKey: "k2", BackendName: "b1", SizeBytes: 20},
	}})
	if _, err := DeleteObjectsBatch(context.Background(), &stubRunner{tx: stub}, []string{"k2", "k1"}); err != nil {
		t.Fatalf("DeleteObjectsBatch: %v", err)
	}
	if len(stub.tagKeysCleared) != 1 {
		t.Fatalf("expected one batch clear statement, got %d: %v", len(stub.tagKeysCleared), stub.tagKeysCleared)
	}
	if len(stub.tagKeysCleared[0]) != 2 {
		t.Errorf("expected both keys cleared, got %v", stub.tagKeysCleared[0])
	}
}

// TestDeleteObjectsBatch_LockError verifies a failure to take one of the key
// locks aborts before any row is read, so the batch cannot half-apply.
func TestDeleteObjectsBatch_LockError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("lock failed")
	stub := newBatchStub(&batchTxStub{quotaTxStub: &quotaTxStub{keyLockErr: sentinel}})
	if _, err := DeleteObjectsBatch(context.Background(), &stubRunner{tx: stub}, []string{"k1"}); !errors.Is(err, sentinel) {
		t.Errorf("expected the lock error, got %v", err)
	}
}

// TestDeleteObjectsBatch_TagClearError verifies a failed tag clear surfaces so
// the whole batch rolls back rather than committing the row deletes and
// leaving the tags for a later object at those keys to inherit.
func TestDeleteObjectsBatch_TagClearError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("tag clear failed")
	stub := newBatchStub(&batchTxStub{
		quotaTxStub: &quotaTxStub{tagClearErr: sentinel},
		keyed:       []KeyedExistingCopy{{ObjectKey: "k1", BackendName: "b1", SizeBytes: 10}},
	})
	if _, err := DeleteObjectsBatch(context.Background(), &stubRunner{tx: stub}, []string{"k1"}); !errors.Is(err, sentinel) {
		t.Errorf("expected the tag clear error, got %v", err)
	}
	if len(stub.ops) != 0 {
		t.Errorf("quota applied despite the tag clear failing: %v", stub.ops)
	}
}

// TestRecordObject_ClearsTagsOnWrite verifies a write resets the key's tag
// set. A PUT is a full replacement, so the object landing here starts with no
// tags rather than inheriting the previous occupant's.
func TestRecordObject_ClearsTagsOnWrite(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{}
	if _, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{Key: "k", Backend: "b1", Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if len(stub.tagsCleared) != 1 || stub.tagsCleared[0] != "k" {
		t.Errorf("expected the key's tags cleared on write, got %v", stub.tagsCleared)
	}
}

// TestRecordObject_TagClearError verifies a failed clear aborts the write.
// Committing past it would leave the new object carrying the previous one's
// tags, which is the inheritance this clear exists to prevent.
func TestRecordObject_TagClearError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("tag clear failed")
	stub := &quotaTxStub{tagClearErr: sentinel}
	if _, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{Key: "k", Backend: "b1", Size: 100}); !errors.Is(err, sentinel) {
		t.Errorf("expected the tag clear error, got %v", err)
	}
	if len(stub.ops) != 0 {
		t.Errorf("quota applied despite the tag clear failing: %v", stub.ops)
	}
}

// TestDeleteObjectLocation_LockError verifies the key lock is taken ahead of
// the row read, so a lock failure stops the call before it reads anything.
func TestDeleteObjectLocation_LockError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("lock failed")
	stub := newExcessStub(&excessTxStub{lockErr: sentinel})
	if err := runDeleteLocation(stub, "k", "b1"); !errors.Is(err, sentinel) {
		t.Errorf("expected the lock error, got %v", err)
	}
	if len(stub.deleted) != 0 {
		t.Errorf("deleted without the lock: %v", stub.deleted)
	}
}

// TestDeleteObject_ClearsTags verifies removing every copy takes the tags with
// it, and that a clear failure surfaces rather than being swallowed.
func TestDeleteObject_ClearsTags(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{{BackendName: "b1", SizeBytes: 100}}})
	if _, err := DeleteObject(context.Background(), &stubRunner{tx: stub}, "k"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if len(stub.tagsCleared) != 1 || stub.tagsCleared[0] != "k" {
		t.Errorf("expected tags cleared with the object, got %v", stub.tagsCleared)
	}
}

// TestDeleteObject_LockError verifies the key lock is taken ahead of the row
// read, so a lock failure stops the delete before it reads anything.
func TestDeleteObject_LockError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("lock failed")
	stub := newExcessStub(&excessTxStub{lockErr: sentinel})
	if _, err := DeleteObject(context.Background(), &stubRunner{tx: stub}, "k"); !errors.Is(err, sentinel) {
		t.Errorf("expected the lock error, got %v", err)
	}
}

// TestDeleteObject_TagClearError verifies a failed clear aborts the delete.
func TestDeleteObject_TagClearError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("tag clear failed")
	stub := newExcessStub(&excessTxStub{
		quotaTxStub: &quotaTxStub{tagClearErr: sentinel},
		existing:    []ExistingCopy{{BackendName: "b1", SizeBytes: 100}},
	})
	if _, err := DeleteObject(context.Background(), &stubRunner{tx: stub}, "k"); !errors.Is(err, sentinel) {
		t.Errorf("expected the tag clear error, got %v", err)
	}
	if len(stub.ops) != 0 {
		t.Errorf("quota applied despite the tag clear failing: %v", stub.ops)
	}
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

// TestDeleteObjectLocation_KeepsTagsWhileCopiesRemain pins the case that makes
// the cascade conditional: tags belong to the object, so removing one replica
// of a multi-copy object must leave them alone. Dropping them here would be
// silent data loss on an object that still exists.
func TestDeleteObjectLocation_KeepsTagsWhileCopiesRemain(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b1", SizeBytes: 100},
		{BackendName: "b2", SizeBytes: 200},
	}})
	if err := runDeleteLocation(stub, "k", "b1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.tagsCleared) != 0 {
		t.Errorf("tags cleared while a copy on b2 still holds the object: %v", stub.tagsCleared)
	}
}

// TestDeleteObjectLocation_ClearsTagsOnLastCopy verifies the other side: once
// the last copy goes the object is gone, so its tags go with it rather than
// being left for a later object at the same key to inherit.
func TestDeleteObjectLocation_ClearsTagsOnLastCopy(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b1", SizeBytes: 100},
	}})
	if err := runDeleteLocation(stub, "k", "b1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.tagsCleared) != 1 || stub.tagsCleared[0] != "k" {
		t.Errorf("expected tags for %q cleared with the last copy, got %v", "k", stub.tagsCleared)
	}
}

// TestDeleteObjectLocation_NoTagClearWhenCopyGone verifies the benign no-op
// path leaves tags alone too: the copy was already absent, so this call
// removed nothing and has no business clearing the object's tags.
func TestDeleteObjectLocation_NoTagClearWhenCopyGone(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b2", SizeBytes: 200},
	}})
	if err := runDeleteLocation(stub, "k", "b1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.tagsCleared) != 0 {
		t.Errorf("tags cleared on a no-op delete: %v", stub.tagsCleared)
	}
}

// TestDeleteObjectLocation_TagClearError verifies a failed tag clear surfaces
// rather than being swallowed. Continuing past it would commit the removal of
// the object's last copy while leaving its tags behind, which the next object
// written to the key would then inherit.
func TestDeleteObjectLocation_TagClearError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("tag clear failed")
	stub := newExcessStub(&excessTxStub{
		quotaTxStub: &quotaTxStub{tagClearErr: sentinel},
		existing:    []ExistingCopy{{BackendName: "b1", SizeBytes: 100}},
	})
	if err := runDeleteLocation(stub, "k", "b1"); !errors.Is(err, sentinel) {
		t.Errorf("expected tag clear error, got %v", err)
	}
	if len(stub.ops) != 0 {
		t.Errorf("quota debited despite the tag clear failing: %v", stub.ops)
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

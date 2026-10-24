// -------------------------------------------------------------------------------
// Core DeleteObjectLocation - Surplus-Copy Branch Tests
//
// Author: Alex Freidah
//
// Engine-agnostic coverage for DeleteObjectLocation with keepAtLeast set, the
// path over-replication cleanup and drain take: the lock and re-read failures,
// the victim-no-longer-in-locked-set and back-at-target refusals, the
// delete/quota mutation failures, and the last-DEK guard. excessTxStub embeds
// quotaTxStub so only the methods the removal touches carry real fixtures.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"testing"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// excessTxStub drives DeleteObjectLocation: it overrides the lock, re-read,
// and delete methods, and inherits DecrementBackendQuota (with its
// failOn/failErr hooks) from the embedded quotaTxStub.
type excessTxStub struct {
	*quotaTxStub
	lockErr     error
	existing    []ExistingCopy
	existingErr error
	deleteErr   error
	deleted     []string
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// AcquireKeyLock returns the seeded lock error (nil = success).
func (s *excessTxStub) AcquireKeyLock(context.Context, string) error { return s.lockErr }

// GetExistingCopiesForUpdate returns the seeded copy set or error.
func (s *excessTxStub) GetExistingCopiesForUpdate(context.Context, string) ([]ExistingCopy, error) {
	return s.existing, s.existingErr
}

// DeleteObjectFromBackend records the deleted backend or returns the seeded error.
func (s *excessTxStub) DeleteObjectFromBackend(_ context.Context, _, backend string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, backend)
	return nil
}

// newExcessStub builds a stub with the embedded quota stub initialized.
func newExcessStub(s *excessTxStub) *excessTxStub {
	if s.quotaTxStub == nil {
		s.quotaTxStub = &quotaTxStub{}
	}
	return s
}

// runRemoveExcess removes key's copy on backend while more than keepAtLeast
// copies remain, reporting only whether a copy went; the byte count and path
// have their own assertions.
func runRemoveExcess(stub *excessTxStub, key, backend string, keepAtLeast int) (bool, error) {
	loc := &ObjectLocation{ObjectKey: key, BackendName: backend}
	_, err := stubOps(stub).DeleteObjectLocation(context.Background(), loc, keepAtLeast, "")
	return err == nil, err
}

// TestRemoveExcessCopy_LockError verifies a failure to take the key lock
// surfaces verbatim and removes nothing.
func TestRemoveExcessCopy_LockError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("lock failed")
	stub := newExcessStub(&excessTxStub{lockErr: sentinel})
	removed, err := runRemoveExcess(stub, "k", "b1", 1)
	if !errors.Is(err, sentinel) {
		t.Errorf("expected lock error, got %v", err)
	}
	if removed {
		t.Error("removed must be false when the lock fails")
	}
}

// TestRemoveExcessCopy_ReadError verifies a failure to re-read the copy
// set under lock surfaces verbatim and removes nothing.
func TestRemoveExcessCopy_ReadError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("read failed")
	stub := newExcessStub(&excessTxStub{existingErr: sentinel})
	removed, err := runRemoveExcess(stub, "k", "b1", 1)
	if !errors.Is(err, sentinel) {
		t.Errorf("expected read error, got %v", err)
	}
	if removed {
		t.Error("removed must be false when the re-read fails")
	}
}

// TestRemoveExcessCopy_VictimNotInLockedSet pins the refusal where the
// object is still over-replicated but the scheduled victim's copy is no
// longer present in the locked set (a parallel path removed exactly that
// copy). We must not delete a different backend's copy.
func TestRemoveExcessCopy_VictimNotInLockedSet(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b1", SizeBytes: 100},
		{BackendName: "b2", SizeBytes: 100},
	}})
	_, err := runRemoveExcess(stub, "k", "b3", 1)
	if !errors.Is(err, ErrCopyChanged) {
		t.Fatalf("expected ErrCopyChanged, got %v", err)
	}
	if len(stub.deleted) != 0 {
		t.Errorf("no copy should be deleted, got %v", stub.deleted)
	}
}

// TestRemoveExcessCopy_BackAtTarget verifies a concurrent removal that already
// brought the object down to keepAtLeast copies stops this one: the victim is
// present, but removing it would leave the object under its target.
func TestRemoveExcessCopy_BackAtTarget(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b1", SizeBytes: 100},
		{BackendName: "b2", SizeBytes: 100},
	}})
	_, err := runRemoveExcess(stub, "k", "b1", 2)
	if !errors.Is(err, ErrCopyNotExcess) {
		t.Fatalf("expected ErrCopyNotExcess, got %v", err)
	}
	if len(stub.deleted) != 0 {
		t.Errorf("no copy should be deleted, got %v", stub.deleted)
	}
}

// TestRemoveExcessCopy_DeleteError verifies a failure to delete the
// metadata row surfaces verbatim and reports nothing removed.
func TestRemoveExcessCopy_DeleteError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("delete failed")
	stub := newExcessStub(&excessTxStub{
		existing:  []ExistingCopy{{BackendName: "b1", SizeBytes: 100}, {BackendName: "b2", SizeBytes: 100}},
		deleteErr: sentinel,
	})
	removed, err := runRemoveExcess(stub, "k", "b1", 1)
	if !errors.Is(err, sentinel) {
		t.Errorf("expected delete error, got %v", err)
	}
	if removed {
		t.Error("removed must be false when the delete fails")
	}
}

// TestRemoveExcessCopy_RemovesVictimWithLockedSize verifies the success
// path: the victim row is deleted, and the bytes queued are the ones on the
// locked row rather than caller-supplied values.
func TestRemoveExcessCopy_RemovesVictimWithLockedSize(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b1", StorageKey: "k!a", SizeBytes: 100},
		{BackendName: "b2", StorageKey: "k!b", SizeBytes: 200},
		{BackendName: "b3", StorageKey: "k!c", SizeBytes: 300},
	}})
	loc := &ObjectLocation{ObjectKey: "k", BackendName: "b1", StorageKey: "k!a", SizeBytes: 1}
	dropped, err := stubOps(stub).DeleteObjectLocation(context.Background(), loc, 2, "over_replication")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.deleted) != 1 || stub.deleted[0] != "b1" {
		t.Errorf("expected b1 deleted, got %v", stub.deleted)
	}
	if len(dropped) != 1 {
		t.Fatalf("queued %d rows, want 1", len(dropped))
	}
	if got := dropped[0]; got.SizeBytes != 100 || got.StorageKey != "k!a" || got.BackendName != "b1" || got.Reason != "over_replication" {
		t.Errorf("queued = %+v, want b1 at k!a, 100 bytes, over_replication", got)
	}
}

// decryptable builds a copy that both claims encryption and still holds a key.
func decryptable(backend string) ExistingCopy {
	return ExistingCopy{BackendName: backend, SizeBytes: 10, Form: StoredReadable}
}

// TestRemoveExcessCopy_RefusesLastDecryptableCopy verifies a copy set that
// disagrees about encryption never loses the one copy that can still be read.
func TestRemoveExcessCopy_RefusesLastDecryptableCopy(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		decryptable("b1"),
		{BackendName: "b2", SizeBytes: 10},
		{BackendName: "b3", SizeBytes: 10},
	}})
	removed, err := runRemoveExcess(stub, "k", "b1", 1)
	if !errors.Is(err, ErrCopyHoldsOnlyDEK) {
		t.Errorf("expected ErrCopyHoldsOnlyDEK, got %v", err)
	}
	if removed {
		t.Error("removed must be false when the victim holds the only key")
	}
	if len(stub.deleted) != 0 {
		t.Errorf("nothing may be deleted, got %v", stub.deleted)
	}
}

// TestRemoveExcessCopy_RemovesKeylessSiblingFromMixedSet verifies the guard
// only protects the victim that holds the key: the copies that lost their
// metadata are still removable, which is what makes the set self-correcting.
func TestRemoveExcessCopy_RemovesKeylessSiblingFromMixedSet(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		decryptable("b1"),
		{BackendName: "b2", SizeBytes: 10},
		{BackendName: "b3", SizeBytes: 10},
	}})
	removed, err := runRemoveExcess(stub, "k", "b2", 1)
	if err != nil {
		t.Fatalf("removing a keyless sibling must succeed, got %v", err)
	}
	if !removed {
		t.Error("removed must be true for a keyless sibling")
	}
	if len(stub.deleted) != 1 || stub.deleted[0] != "b2" {
		t.Errorf("expected b2 deleted, got %v", stub.deleted)
	}
}

// TestRemoveExcessCopy_AllowsRemovalWhenEveryCopyDecryptable verifies the
// guard does not fire on a consistent encrypted set, where every remaining
// copy can still read the object.
func TestRemoveExcessCopy_AllowsRemovalWhenEveryCopyDecryptable(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		decryptable("b1"), decryptable("b2"), decryptable("b3"),
	}})
	removed, err := runRemoveExcess(stub, "k", "b1", 1)
	if err != nil {
		t.Fatalf("a fully encrypted set must stay removable, got %v", err)
	}
	if !removed {
		t.Error("removed must be true when every copy carries a key")
	}
}

// TestRemoveExcessCopy_AllowsRemovalWhenNoCopyEncrypted verifies an entirely
// unencrypted set is untouched by the guard.
func TestRemoveExcessCopy_AllowsRemovalWhenNoCopyEncrypted(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b1", SizeBytes: 10},
		{BackendName: "b2", SizeBytes: 10},
	}})
	removed, err := runRemoveExcess(stub, "k", "b1", 1)
	if err != nil {
		t.Fatalf("an unencrypted set must stay removable, got %v", err)
	}
	if !removed {
		t.Error("removed must be true when no copy is encrypted")
	}
}

// TestRemoveExcessCopy_AllowsRemovalWhenEncryptedCopyLostItsKey verifies a
// copy flagged encrypted but missing its key is not mistaken for the last
// decryptable one: it cannot read the object either, so it stays removable.
func TestRemoveExcessCopy_AllowsRemovalWhenEncryptedCopyLostItsKey(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b1", SizeBytes: 10, Form: StoredEncrypted},
		{BackendName: "b2", SizeBytes: 10},
	}})
	removed, err := runRemoveExcess(stub, "k", "b1", 1)
	if err != nil {
		t.Fatalf("a keyless encrypted copy must stay removable, got %v", err)
	}
	if !removed {
		t.Error("removed must be true when the encrypted copy has no key")
	}
}

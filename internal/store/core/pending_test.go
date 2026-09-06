// -------------------------------------------------------------------------------
// Core Pending Helpers - Pure-Function Tests
//
// Author: Alex Freidah
//
// Engine-agnostic coverage for intentSuperseded and pendingStoredForm.
// Both helpers are exercised exhaustively here; the per-engine adapter layers
// only need integration coverage that the right values reach these helpers.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// PROMOTION STUB
// -------------------------------------------------------------------------

// promoteTxStub records the order a promotion touches the transaction in, which
// is what the lock-ordering guarantee is made of, along with the rows it wrote
// and the intents it removed.
type promoteTxStub struct {
	noopTxAdapter

	existingCopies []ExistingCopy
	claimed        bool

	keyLockErr  error
	claimErr    error
	existingErr error
	deleteErr   error

	ops      []string
	inserted []ObjectLocation
	deleted  []string
	cleared  [][]string
}

func (s *promoteTxStub) AcquireKeyLock(context.Context, string) error {
	s.ops = append(s.ops, "key_lock")
	return s.keyLockErr
}

func (s *promoteTxStub) ClaimPending(context.Context, string) (bool, error) {
	s.ops = append(s.ops, "claim")
	return s.claimed, s.claimErr
}

func (s *promoteTxStub) GetExistingCopiesForUpdate(context.Context, string) ([]ExistingCopy, error) {
	s.ops = append(s.ops, "read_copies")
	return s.existingCopies, s.existingErr
}

func (s *promoteTxStub) InsertObjectLocation(_ context.Context, loc *ObjectLocation) error {
	s.ops = append(s.ops, "insert")
	s.inserted = append(s.inserted, *loc)
	return nil
}

func (s *promoteTxStub) DeletePending(_ context.Context, intentID string) error {
	s.ops = append(s.ops, "delete_pending")
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, intentID)
	return nil
}

func (s *promoteTxStub) ClearPendingForKey(_ context.Context, _ string, keep []string) ([]SupersededIntent, error) {
	s.cleared = append(s.cleared, keep)
	return nil, nil
}

// companionIntent is the fixture the companion tests resolve.
func companionIntent() *PendingObject {
	return &PendingObject{
		IntentID:    "i-1",
		ObjectKey:   "k",
		BackendName: "b2",
		SizeBytes:   100,
		Role:        PendingRoleCompanion,
	}
}

// -------------------------------------------------------------------------
// PendingObject role
// -------------------------------------------------------------------------

// TestPendingRole_DefaultsToPrimary verifies an intent written without a role
// stores the value the column defaults to, rather than an empty string the
// CHECK constraint would refuse.
func TestPendingRole_DefaultsToPrimary(t *testing.T) {
	t.Parallel()
	var p PendingObject
	if got := p.RoleOrDefault(); got != PendingRolePrimary {
		t.Errorf("RoleOrDefault on an unset role = %q, want %q", got, PendingRolePrimary)
	}
	if p.IsCompanion() {
		t.Error("an unset role must not read as a companion")
	}
	p.Role = PendingRoleCompanion
	if got := p.RoleOrDefault(); got != PendingRoleCompanion {
		t.Errorf("RoleOrDefault = %q, want %q", got, PendingRoleCompanion)
	}
	if !p.IsCompanion() {
		t.Error("a companion role must read as a companion")
	}
}

// -------------------------------------------------------------------------
// promotePendingTx - lock ordering and companion resolution
// -------------------------------------------------------------------------

// TestPromotePending_LocksKeyBeforeClaiming verifies the object-key lock is
// taken before the intent row is claimed. A write takes that lock and then
// deletes the key's intent rows, so claiming first would have the two
// transactions each waiting on what the other holds.
func TestPromotePending_LocksKeyBeforeClaiming(t *testing.T) {
	t.Parallel()
	stub := &promoteTxStub{claimed: true}
	if _, _, _, err := PromotePending(context.Background(), &stubRunner{tx: stub}, companionIntent()); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if len(stub.ops) < 2 || stub.ops[0] != "key_lock" || stub.ops[1] != "claim" {
		t.Errorf("expected the key lock before the claim, got %v", stub.ops)
	}
}

// TestPromotePending_CompanionKeptWhenCopyRecorded verifies that an extra-copy
// intent whose backend already holds a recorded copy leaves the bytes alone:
// they are that copy, not the intent's.
func TestPromotePending_CompanionKeptWhenCopyRecorded(t *testing.T) {
	t.Parallel()
	stub := &promoteTxStub{
		claimed:        true,
		existingCopies: []ExistingCopy{{BackendName: "b1", SizeBytes: 100}, {BackendName: "b2", SizeBytes: 100}},
	}
	result, displaced, _, err := PromotePending(context.Background(), &stubRunner{tx: stub}, companionIntent())
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != PendingPromoteCompanionKept {
		t.Errorf("result = %v, want CompanionKept", result)
	}
	if len(displaced) != 0 {
		t.Errorf("expected no bytes removed, got %+v", displaced)
	}
	if len(stub.inserted) != 0 {
		t.Errorf("companion promotion must not record a copy, got %+v", stub.inserted)
	}
	if len(stub.deleted) != 1 || stub.deleted[0] != "i-1" {
		t.Errorf("expected the intent cleared, got %v", stub.deleted)
	}
}

// TestPromotePending_CompanionDiscardedWhenNoCopy verifies that an extra-copy
// intent with nothing recorded on its backend hands those bytes back for
// removal rather than recording a copy nothing can vouch for.
func TestPromotePending_CompanionDiscardedWhenNoCopy(t *testing.T) {
	t.Parallel()
	stub := &promoteTxStub{
		claimed:        true,
		existingCopies: []ExistingCopy{{BackendName: "b1", SizeBytes: 100}},
	}
	result, displaced, _, err := PromotePending(context.Background(), &stubRunner{tx: stub}, companionIntent())
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != PendingPromoteCompanionDiscarded {
		t.Errorf("result = %v, want CompanionDiscarded", result)
	}
	if len(displaced) != 1 || displaced[0].BackendName != "b2" || displaced[0].SizeBytes != 100 {
		t.Fatalf("expected the intent's bytes handed back, got %+v", displaced)
	}
	if displaced[0].Reason != CleanupReasonCompanionDiscarded {
		t.Errorf("reason = %q, want %q", displaced[0].Reason, CleanupReasonCompanionDiscarded)
	}
	if len(stub.inserted) != 0 {
		t.Errorf("companion promotion must not record a copy, got %+v", stub.inserted)
	}
}

// TestPromotePending_PrimaryReplacesTheCopySet verifies a primary intent keeps
// its meaning: promoting it records the object and clears the copies the write
// it belongs to was replacing.
func TestPromotePending_PrimaryReplacesTheCopySet(t *testing.T) {
	t.Parallel()
	stub := &promoteTxStub{
		claimed:        true,
		existingCopies: []ExistingCopy{{BackendName: "b1", SizeBytes: 50}},
	}
	intent := companionIntent()
	intent.Role = PendingRolePrimary

	result, displaced, deltas, err := PromotePending(context.Background(), &stubRunner{tx: stub}, intent)
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != PendingPromoteCommitted {
		t.Fatalf("result = %v, want Committed", result)
	}
	if len(stub.inserted) != 1 || stub.inserted[0].BackendName != "b2" {
		t.Errorf("expected the promoted copy recorded on b2, got %+v", stub.inserted)
	}
	if len(displaced) != 1 || displaced[0].BackendName != "b1" {
		t.Errorf("expected the prior copy displaced, got %+v", displaced)
	}
	if deltas["b2"] != 100 || deltas["b1"] != -50 {
		t.Errorf("deltas = %+v, want b2:+100 b1:-50", deltas)
	}
}

// TestPromotePending_PrimarySupersededByNewerCopy verifies the timestamp check
// still refuses to promote a primary intent an existing newer copy has
// overtaken, which is the insurance behind writes clearing their own intents.
func TestPromotePending_PrimarySupersededByNewerCopy(t *testing.T) {
	t.Parallel()
	intent := companionIntent()
	intent.Role = PendingRolePrimary
	intent.CreatedAt = time.Now().Add(-time.Minute)
	stub := &promoteTxStub{
		claimed:        true,
		existingCopies: []ExistingCopy{{BackendName: "b1", SizeBytes: 50, CreatedAt: time.Now()}},
	}

	result, displaced, _, err := PromotePending(context.Background(), &stubRunner{tx: stub}, intent)
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != PendingPromoteSuperseded {
		t.Errorf("result = %v, want Superseded", result)
	}
	if len(stub.inserted) != 0 || len(displaced) != 0 {
		t.Errorf("a superseded intent must record nothing: inserted=%+v displaced=%+v", stub.inserted, displaced)
	}
}

// TestPromotePending_SurfacesTransactionErrors verifies each step's failure
// aborts the promotion rather than resolving the intent on a guess.
func TestPromotePending_SurfacesTransactionErrors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	cases := map[string]*promoteTxStub{
		"key lock":    {keyLockErr: sentinel},
		"claim":       {claimErr: sentinel},
		"read copies": {claimed: true, existingErr: sentinel},
	}
	for name, stub := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, _, err := PromotePending(context.Background(), &stubRunner{tx: stub}, companionIntent())
			if !errors.Is(err, sentinel) {
				t.Errorf("expected the %s error, got %v", name, err)
			}
			if len(stub.deleted) != 0 {
				t.Errorf("intent resolved despite the failure: %v", stub.deleted)
			}
		})
	}
}

// TestPromotePending_CompanionDeleteFailureAborts verifies a companion that
// cannot clear its own row fails the transaction rather than reporting bytes
// for deletion while the intent survives to claim them again.
func TestPromotePending_CompanionDeleteFailureAborts(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("delete failed")
	stub := &promoteTxStub{claimed: true, deleteErr: sentinel}

	_, displaced, _, err := PromotePending(context.Background(), &stubRunner{tx: stub}, companionIntent())
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the delete error, got %v", err)
	}
	if len(displaced) != 0 {
		t.Errorf("bytes reported for removal despite the failure: %+v", displaced)
	}
}

// TestPromotePending_CompanionAlreadyResolved verifies a claim that finds no
// row is a benign no-op: another instance, or the write itself, settled it.
func TestPromotePending_CompanionAlreadyResolved(t *testing.T) {
	t.Parallel()
	stub := &promoteTxStub{claimed: false}
	result, _, _, err := PromotePending(context.Background(), &stubRunner{tx: stub}, companionIntent())
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != PendingPromoteAlreadyResolved {
		t.Errorf("result = %v, want AlreadyResolved", result)
	}
	if slices.Contains(stub.ops, "delete_pending") {
		t.Errorf("an unclaimed intent must not be deleted, got %v", stub.ops)
	}
}

// -------------------------------------------------------------------------
// pendingStoredForm
// -------------------------------------------------------------------------

// TestPendingStoredForm_ReturnsNilWhenUnencryptedAndNoHash verifies
// the helper returns nil for plain unencrypted PUTs with no content hash,
// so the promoted object_locations row mirrors the unencrypted shape.
func TestPendingStoredForm_ReturnsNilWhenUnencryptedAndNoHash(t *testing.T) {
	t.Parallel()
	if got := pendingStoredForm(&PendingObject{}); got != nil {
		t.Errorf("expected nil for unencrypted no-hash pending, got %+v", got)
	}
}

// TestPendingStoredForm_PreservesEncryptionFields verifies that an
// encrypted pending intent projects every encryption attribute onto the
// StoredForm the promote path hands to RecordObject.
func TestPendingStoredForm_PreservesEncryptionFields(t *testing.T) {
	t.Parallel()
	p := PendingObject{
		Encrypted:     true,
		EncryptionKey: []byte("packed"),
		KeyID:         "kid-1",
		PlaintextSize: 90,
		ContentHash:   "hash",
	}
	got := pendingStoredForm(&p)
	if got == nil {
		t.Fatal("expected non-nil meta")
	}
	if !got.Encrypted || got.KeyID != "kid-1" || got.PlaintextSize != 90 || got.ContentHash != "hash" {
		t.Errorf("meta mismatch: %+v", got)
	}
	if string(got.EncryptionKey) != "packed" {
		t.Errorf("EncryptionKey not propagated: %v", got.EncryptionKey)
	}
}

// TestPendingStoredForm_HashOnlyReturnsForm verifies that an
// integrity-only PUT (encryption disabled, content hash present) still
// produces a non-nil StoredForm so the hash survives promotion.
func TestPendingStoredForm_HashOnlyReturnsForm(t *testing.T) {
	t.Parallel()
	got := pendingStoredForm(&PendingObject{ContentHash: "hash"})
	if got == nil {
		t.Fatal("expected non-nil meta when hash is set")
	}
	if got.Encrypted || got.ContentHash != "hash" {
		t.Errorf("meta mismatch: %+v", got)
	}
}

// -------------------------------------------------------------------------
// intentSuperseded - pure function, drives the timestamp-aware drop path
// -------------------------------------------------------------------------

// TestIntentSuperseded_NoExistingCopiesNeverSuperseded verifies the empty
// case: with no existing rows there is nothing newer than the intent.
func TestIntentSuperseded_NoExistingCopiesNeverSuperseded(t *testing.T) {
	t.Parallel()
	if intentSuperseded(nil, time.Now()) {
		t.Error("empty existing slice must not be superseded")
	}
}

// TestIntentSuperseded_AllOlderNeverSuperseded verifies the canonical
// promote case: the intent is at least as new as every existing copy.
func TestIntentSuperseded_AllOlderNeverSuperseded(t *testing.T) {
	t.Parallel()
	intentTime := time.Now()
	existing := []ExistingCopy{
		{BackendName: "b1", CreatedAt: intentTime.Add(-1 * time.Minute)},
		{BackendName: "b2", CreatedAt: intentTime.Add(-2 * time.Minute)},
	}
	if intentSuperseded(existing, intentTime) {
		t.Error("all-older existing rows must not trigger supersession")
	}
}

// TestIntentSuperseded_AnyNewerSupersedes verifies that even one row newer
// than the intent fires the drop path. This is the head-of-line-blocking
// fix: a successful retry on any backend authoritatively supersedes the
// stale intent.
func TestIntentSuperseded_AnyNewerSupersedes(t *testing.T) {
	t.Parallel()
	intentTime := time.Now()
	existing := []ExistingCopy{
		{BackendName: "b1", CreatedAt: intentTime.Add(-1 * time.Minute)},
		{BackendName: "b2", CreatedAt: intentTime.Add(1 * time.Minute)},
	}
	if !intentSuperseded(existing, intentTime) {
		t.Error("any newer existing row must trigger supersession")
	}
}

// TestIntentSuperseded_EqualTimePromotes verifies that ties go to promote.
// The intent is "at least as new" as the existing row; only strictly
// newer existing rows count as supersession.
func TestIntentSuperseded_EqualTimePromotes(t *testing.T) {
	t.Parallel()
	intentTime := time.Now()
	existing := []ExistingCopy{
		{BackendName: "b1", CreatedAt: intentTime},
	}
	if intentSuperseded(existing, intentTime) {
		t.Error("equal timestamp must not trigger supersession")
	}
}

// TestIntentSuperseded_ZeroTimestampSkipped verifies that rows with a
// zero CreatedAt are ignored. Engine adapters set CreatedAt to the zero
// value when the underlying database row is NULL/invalid; supersession
// must not fire on those.
func TestIntentSuperseded_ZeroTimestampSkipped(t *testing.T) {
	t.Parallel()
	intentTime := time.Now()
	existing := []ExistingCopy{
		{BackendName: "b1", CreatedAt: time.Time{}},
	}
	if intentSuperseded(existing, intentTime) {
		t.Error("zero timestamp must be ignored, not treated as newer")
	}
}

// -------------------------------------------------------------------------
// COMPANION COMMIT
// -------------------------------------------------------------------------

// companionTxStub drives a companion commit: it decides whether the intent is
// still there, what the backend already holds for the key, and records what the
// commit wrote or removed.
type companionTxStub struct {
	noopTxAdapter

	claimed    bool
	claimErr   error
	lockedCopy *ObjectLocation

	keyLockErr error
	insertErr  error
	deleteErr  error
	pullErr    error

	inserted   []ObjectLocation
	deleted    []string
	rowsPulled []string
	stripes    []int64
}

func (s *companionTxStub) AcquireKeyLock(context.Context, string) error { return s.keyLockErr }

func (s *companionTxStub) ClaimPending(context.Context, string) (bool, error) {
	return s.claimed, s.claimErr
}

func (s *companionTxStub) InsertObjectLocation(_ context.Context, loc *ObjectLocation) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.inserted = append(s.inserted, *loc)
	return nil
}

func (s *companionTxStub) DeletePending(_ context.Context, intentID string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, intentID)
	return nil
}

// LockObjectOnBackend reports whatever copy the test seeded on the backend.
func (s *companionTxStub) LockObjectOnBackend(context.Context, string, string) (*ObjectLocation, bool, error) {
	return s.lockedCopy, s.lockedCopy != nil, nil
}

// DeleteObjectFromBackend records the row the discard removed.
func (s *companionTxStub) DeleteObjectFromBackend(_ context.Context, _, backend string) error {
	if s.pullErr != nil {
		return s.pullErr
	}
	s.rowsPulled = append(s.rowsPulled, backend)
	return nil
}

func (s *companionTxStub) AdjustQuotaStripe(_ context.Context, _ string, _ int16, delta int64) error {
	s.stripes = append(s.stripes, delta)
	return nil
}

// TestCommitCompanionCopy_AddsTheCopy verifies a copy whose intent survived is
// recorded as an addition: the row goes in, the backend is charged, and the
// intent it was admitted on is removed in the same transaction.
func TestCommitCompanionCopy_AddsTheCopy(t *testing.T) {
	t.Parallel()
	stub := &companionTxStub{claimed: true}
	p := &PendingObject{IntentID: "i-2", ObjectKey: "k", BackendName: "b2", SizeBytes: 100, Role: PendingRoleCompanion}

	result, displaced, deltas, err := CommitCompanionCopy(context.Background(), &stubRunner{tx: stub}, p)
	if err != nil {
		t.Fatalf("CommitCompanionCopy: %v", err)
	}
	if result != CompanionCopyCommitted {
		t.Errorf("result = %v, want committed", result)
	}
	if len(stub.inserted) != 1 || stub.inserted[0].BackendName != "b2" {
		t.Fatalf("expected one row on b2, got %+v", stub.inserted)
	}
	if deltas["b2"] != 100 {
		t.Errorf("delta for b2 = %d, want 100", deltas["b2"])
	}
	if !slices.Equal(stub.deleted, []string{"i-2"}) {
		t.Errorf("intents removed = %v, want the committed one", stub.deleted)
	}
	if len(displaced) != 0 {
		t.Errorf("a committed copy displaced %+v, want nothing", displaced)
	}
}

// TestCommitCompanionCopy_DiscardsWhenTheIntentIsGone verifies a copy whose
// intent a newer write cleared is not recorded. Its bytes went down at a path
// that write may also have written, in an order nothing here can establish, so
// the copy is handed back for removal instead.
func TestCommitCompanionCopy_DiscardsWhenTheIntentIsGone(t *testing.T) {
	t.Parallel()
	stub := &companionTxStub{claimed: false}
	p := &PendingObject{IntentID: "i-2", ObjectKey: "k", BackendName: "b2", SizeBytes: 100, Role: PendingRoleCompanion}

	result, displaced, _, err := CommitCompanionCopy(context.Background(), &stubRunner{tx: stub}, p)
	if err != nil {
		t.Fatalf("CommitCompanionCopy: %v", err)
	}
	if result != CompanionCopyUntrusted {
		t.Errorf("result = %v, want untrusted", result)
	}
	if len(stub.inserted) != 0 {
		t.Errorf("recorded %+v for a copy it could not vouch for", stub.inserted)
	}
	if len(displaced) != 1 || displaced[0].BackendName != "b2" ||
		displaced[0].Reason != CleanupReasonCompanionUntrusted {
		t.Fatalf("displaced = %+v, want b2 labelled companion_untrusted", displaced)
	}
	if displaced[0].SizeBytes != 100 {
		t.Errorf("displaced size = %d, want the intent's 100 when no row existed", displaced[0].SizeBytes)
	}
}

// TestCommitCompanionCopy_DropsTheRowItCannotVouchFor verifies the discard also
// removes a recorded copy on that backend. The row describes the same path, so
// it is no safer than the bytes: replication rebuilds the copy from one the
// client was told about.
func TestCommitCompanionCopy_DropsTheRowItCannotVouchFor(t *testing.T) {
	t.Parallel()
	stub := &companionTxStub{
		claimed:    false,
		lockedCopy: &ObjectLocation{ObjectKey: "k", BackendName: "b2", SizeBytes: 250},
	}
	p := &PendingObject{IntentID: "i-2", ObjectKey: "k", BackendName: "b2", SizeBytes: 100, Role: PendingRoleCompanion}

	_, displaced, deltas, err := CommitCompanionCopy(context.Background(), &stubRunner{tx: stub}, p)
	if err != nil {
		t.Fatalf("CommitCompanionCopy: %v", err)
	}
	if !slices.Equal(stub.rowsPulled, []string{"b2"}) {
		t.Errorf("rows removed = %v, want b2's", stub.rowsPulled)
	}
	if deltas["b2"] != -250 {
		t.Errorf("delta for b2 = %d, want the row's 250 credited back", deltas["b2"])
	}
	if len(displaced) != 1 || displaced[0].SizeBytes != 250 {
		t.Fatalf("displaced = %+v, want the recorded copy's size", displaced)
	}
}

// TestCommitCompanionCopy_ClaimErrorSurfaces verifies a database failure is
// reported rather than read as a missing intent, since reading it that way
// would delete a copy over an outage.
func TestCommitCompanionCopy_ClaimErrorSurfaces(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("db down")
	stub := &companionTxStub{claimErr: sentinel}
	p := &PendingObject{IntentID: "i-2", ObjectKey: "k", BackendName: "b2", SizeBytes: 100, Role: PendingRoleCompanion}

	_, displaced, _, err := CommitCompanionCopy(context.Background(), &stubRunner{tx: stub}, p)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the claim failure", err)
	}
	if len(displaced) != 0 {
		t.Errorf("displaced %+v on a database error, want nothing deleted", displaced)
	}
}

// TestCommitCompanionCopy_RollsBackOnAFailedStep verifies the commit is all or
// nothing: whichever statement fails, the transaction carries the error out
// rather than leaving a row without its intent cleared, or the reverse.
func TestCommitCompanionCopy_RollsBackOnAFailedStep(t *testing.T) {
	t.Parallel()
	p := &PendingObject{IntentID: "i-2", ObjectKey: "k", BackendName: "b2", SizeBytes: 100, Role: PendingRoleCompanion}

	for name, stub := range map[string]*companionTxStub{
		"key lock":       {keyLockErr: errors.New("lock timeout")},
		"insert":         {claimed: true, insertErr: errors.New("insert failed")},
		"intent removal": {claimed: true, deleteErr: errors.New("delete failed")},
		"row removal": {
			lockedCopy: &ObjectLocation{ObjectKey: "k", BackendName: "b2", SizeBytes: 250},
			pullErr:    errors.New("row delete failed"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, displaced, _, err := CommitCompanionCopy(context.Background(), &stubRunner{tx: stub}, p)
			if err == nil {
				t.Fatalf("expected the %s failure to surface", name)
			}
			if len(displaced) != 0 {
				t.Errorf("displaced %+v on a failed commit, want nothing deleted", displaced)
			}
		})
	}
}

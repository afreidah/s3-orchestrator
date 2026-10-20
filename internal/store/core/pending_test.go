// -------------------------------------------------------------------------------
// Core Pending Helpers - Pure-Function Tests
//
// Author: Alex Freidah
//
// Engine-agnostic coverage for the pending claim, promotion, and
// intentSuperseded. The per-engine adapter layers only need integration
// coverage that the right values reach these helpers.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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

// TestPromotePending_CarriesTheStoredForm verifies the promoted row records the
// stored form the intent was written with, so a recovered object's row matches
// its bytes.
func TestPromotePending_CarriesTheStoredForm(t *testing.T) {
	t.Parallel()
	stub := &promoteTxStub{claimed: true}
	intent := companionIntent()
	intent.Role = PendingRolePrimary
	intent.StoredForm = StoredForm{
		Encrypted: true, EncryptionKey: []byte("packed"), KeyID: "kid-1", PlaintextSize: 90,
		ContentHash: "hash", CompressionAlgorithm: "zstd", CompressionLevel: "default",
		CompressionFormatVersion: 1, LogicalSize: 120,
	}

	if _, _, _, err := PromotePending(context.Background(), &stubRunner{tx: stub}, intent); err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if len(stub.inserted) != 1 {
		t.Fatalf("inserted = %+v, want one row", stub.inserted)
	}
	if got := stub.inserted[0].StoredForm; !reflect.DeepEqual(got, intent.StoredForm) {
		t.Errorf("promoted form = %+v, want %+v", got, intent.StoredForm)
	}
}

// TestPromotePending_PrimarySupersededByNewerCopy verifies the timestamp check
// still refuses to promote a primary intent an existing newer copy has
// overtaken, and that the intent is dropped with its uploaded bytes queued so
// they do not leak.
func TestPromotePending_PrimarySupersededByNewerCopy(t *testing.T) {
	t.Parallel()
	intent := companionIntent()
	intent.Role = PendingRolePrimary
	intent.StorageKey = "k!i-1"
	intent.CreatedAt = time.Now().Add(-time.Minute)
	stub := &promoteTxStub{
		claimed:        true,
		existingCopies: []ExistingCopy{{BackendName: "b1", SizeBytes: 50, CreatedAt: time.Now()}},
	}

	result, cleanup, deltas, err := PromotePending(context.Background(), &stubRunner{tx: stub}, intent)
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != PendingPromoteSuperseded {
		t.Errorf("result = %v, want Superseded", result)
	}
	if len(stub.inserted) != 0 || len(deltas) != 0 {
		t.Errorf("a superseded intent must record nothing: inserted=%+v deltas=%+v", stub.inserted, deltas)
	}
	if !slices.Equal(stub.deleted, []string{"i-1"}) {
		t.Errorf("deleted intents = %v, want the superseded one", stub.deleted)
	}
	want := []CleanupItem{{BackendName: "b2", ObjectKey: "k", StorageKey: "k!i-1", Reason: CleanupReasonSupersededIntent, SizeBytes: 100}}
	if !slices.Equal(cleanup, want) {
		t.Errorf("cleanup = %+v, want %+v", cleanup, want)
	}
}

// TestPromotePending_SupersededDeleteFailureAborts verifies a superseded intent
// that cannot be dropped fails the promotion rather than queueing bytes the
// surviving intent would later claim again.
func TestPromotePending_SupersededDeleteFailureAborts(t *testing.T) {
	t.Parallel()
	intent := companionIntent()
	intent.Role = PendingRolePrimary
	intent.CreatedAt = time.Now().Add(-time.Minute)
	sentinel := errors.New("delete failed")
	stub := &promoteTxStub{
		claimed:        true,
		deleteErr:      sentinel,
		existingCopies: []ExistingCopy{{BackendName: "b1", SizeBytes: 50, CreatedAt: time.Now()}},
	}

	result, cleanup, _, err := PromotePending(context.Background(), &stubRunner{tx: stub}, intent)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the delete failure", err)
	}
	if result != PendingPromoteAmbiguous || cleanup != nil {
		t.Errorf("result = %v cleanup = %+v, want Ambiguous and nothing queued", result, cleanup)
	}
}

// TestCompanion_QueueFailureRollsBack verifies a companion resolution that
// cannot queue the bytes it discards fails, on both the reaper's path and the
// late commit's.
func TestCompanion_QueueFailureRollsBack(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("queue failed")
	reaper := &queueRecorder{TxAdapter: &promoteTxStub{claimed: true}, err: sentinel}
	if _, cleanup, _, err := PromotePending(context.Background(), &stubRunner{tx: reaper}, companionIntent()); !errors.Is(err, sentinel) || cleanup != nil {
		t.Errorf("PromotePending cleanup=%+v err=%v, want the queue failure", cleanup, err)
	}
	late := &queueRecorder{TxAdapter: &companionTxStub{claimed: false}, err: sentinel}
	if _, cleanup, _, err := CommitCompanionCopy(context.Background(), &stubRunner{tx: late}, companionIntent()); !errors.Is(err, sentinel) || cleanup != nil {
		t.Errorf("CommitCompanionCopy cleanup=%+v err=%v, want the queue failure", cleanup, err)
	}
}

// TestPromotePending_CommitFailuresRollBack verifies each statement of a
// committed promotion carries its failure out, including queueing the
// displaced copy's bytes.
func TestPromotePending_CommitFailuresRollBack(t *testing.T) {
	t.Parallel()
	intent := companionIntent()
	intent.Role = PendingRolePrimary
	sentinel := errors.New("step failed")
	existing := []ExistingCopy{{BackendName: "b1", SizeBytes: 50}}
	cases := map[string]TxAdapter{
		"intent delete": &promoteTxStub{claimed: true, existingCopies: existing, deleteErr: sentinel},
		"queue":         &queueRecorder{TxAdapter: &promoteTxStub{claimed: true, existingCopies: existing}, err: sentinel},
	}
	for name, tx := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, _, _, err := PromotePending(context.Background(), &stubRunner{tx: tx}, intent); !errors.Is(err, sentinel) {
				t.Errorf("err = %v, want the %s failure", err, name)
			}
		})
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
// intent a newer write cleared is not recorded, and that the bytes handed back
// for removal are the ones this upload wrote, named by its own path.
func TestCommitCompanionCopy_DiscardsWhenTheIntentIsGone(t *testing.T) {
	t.Parallel()
	stub := &companionTxStub{claimed: false}
	p := &PendingObject{
		IntentID: "i-2", ObjectKey: "k", StorageKey: "k!i-2",
		BackendName: "b2", SizeBytes: 100, Role: PendingRoleCompanion,
	}

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
	if displaced[0].StorageKey != "k!i-2" {
		t.Errorf("displaced path = %q, want the discarded upload's own %q",
			displaced[0].StorageKey, "k!i-2")
	}
	if displaced[0].SizeBytes != 100 {
		t.Errorf("displaced size = %d, want the intent's 100", displaced[0].SizeBytes)
	}
}

// TestCommitCompanionCopy_LeavesTheWinnersRowAlone verifies a losing companion
// that resolves while the key already holds a copy on the same backend,
// recorded by the write that overtook it, leaves that row and its bytes
// untouched and removes only its own path.
func TestCommitCompanionCopy_LeavesTheWinnersRowAlone(t *testing.T) {
	t.Parallel()
	stub := &companionTxStub{
		claimed:    false,
		lockedCopy: &ObjectLocation{ObjectKey: "k", BackendName: "b2", StorageKey: "k!winner", SizeBytes: 250},
	}
	p := &PendingObject{
		IntentID: "i-2", ObjectKey: "k", StorageKey: "k!loser",
		BackendName: "b2", SizeBytes: 100, Role: PendingRoleCompanion,
	}

	_, displaced, deltas, err := CommitCompanionCopy(context.Background(), &stubRunner{tx: stub}, p)
	if err != nil {
		t.Fatalf("CommitCompanionCopy: %v", err)
	}
	if len(stub.rowsPulled) != 0 {
		t.Errorf("rows removed = %v, want the winner's row left alone", stub.rowsPulled)
	}
	if len(deltas) != 0 {
		t.Errorf("deltas = %+v, want nothing charged for bytes never recorded", deltas)
	}
	if len(displaced) != 1 {
		t.Fatalf("displaced = %+v, want exactly the discarded upload's bytes", displaced)
	}
	if displaced[0].StorageKey != "k!loser" {
		t.Errorf("displaced path = %q, want the loser's own %q; %q is the committed copy",
			displaced[0].StorageKey, "k!loser", "k!winner")
	}
	if displaced[0].SizeBytes != 100 {
		t.Errorf("displaced size = %d, want the intent's 100", displaced[0].SizeBytes)
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

// -------------------------------------------------------------------------
// PENDING CLAIM
// -------------------------------------------------------------------------

// claimTxStub records the order a claim touches the transaction in and what
// reached the insert, and answers the insert with a seeded result. A backend
// named in full declines whatever fits says.
type claimTxStub struct {
	noopTxAdapter

	fits      bool
	full      map[string]bool
	lockErr   error
	insertErr error

	ops         []string
	locked      []string
	inserted    []string
	lockedName  string
	insertedID  string
	accepted    BackendFlags
	insertCalls int
}

func (s *claimTxStub) AcquireBackendLock(_ context.Context, backendName string) error {
	s.ops = append(s.ops, "backend_lock")
	s.locked = append(s.locked, backendName)
	s.lockedName = backendName
	return s.lockErr
}

func (s *claimTxStub) InsertPendingIfFits(_ context.Context, p *PendingObject, accepts BackendFlags) (bool, error) {
	s.ops = append(s.ops, "insert")
	s.inserted = append(s.inserted, p.BackendName)
	s.insertCalls++
	s.insertedID = p.IntentID
	s.accepted = accepts
	return s.fits && !s.full[p.BackendName], s.insertErr
}

// claimIntent is the intent every claim test admits.
func claimIntent() *PendingObject {
	return &PendingObject{IntentID: "i-1", ObjectKey: "k", BackendName: "b1", SizeBytes: 100}
}

// TestInsertPendingIfFits_LocksBackendBeforeInsert verifies the backend lock is
// taken on the intent's backend before the insert, and the insert receives the
// intent and the accepted flags unchanged.
func TestInsertPendingIfFits_LocksBackendBeforeInsert(t *testing.T) {
	t.Parallel()
	stub := &claimTxStub{fits: true}
	fits, err := InsertPendingIfFits(context.Background(), &stubRunner{tx: stub}, claimIntent(), AcceptForWrite|FlagCordoned)
	if err != nil || !fits {
		t.Fatalf("InsertPendingIfFits = %v, %v; want fits", fits, err)
	}
	if want := []string{"backend_lock", "insert"}; !slices.Equal(stub.ops, want) {
		t.Errorf("ops = %v, want %v", stub.ops, want)
	}
	if stub.lockedName != "b1" || stub.insertedID != "i-1" {
		t.Errorf("locked %q, inserted %q; want b1 and i-1", stub.lockedName, stub.insertedID)
	}
	if stub.accepted != AcceptForWrite|FlagCordoned {
		t.Errorf("accepts = %v, want %v", stub.accepted, AcceptForWrite|FlagCordoned)
	}
}

// TestInsertPendingIfFits_LockErrorSkipsInsert verifies a failed backend lock
// surfaces verbatim and no intent is recorded.
func TestInsertPendingIfFits_LockErrorSkipsInsert(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("lock timeout")
	stub := &claimTxStub{fits: true, lockErr: sentinel}
	fits, err := InsertPendingIfFits(context.Background(), &stubRunner{tx: stub}, claimIntent(), AcceptForWrite)
	if !errors.Is(err, sentinel) || fits {
		t.Errorf("InsertPendingIfFits = %v, %v; want false and the lock error", fits, err)
	}
	if stub.insertCalls != 0 {
		t.Errorf("insert ran %d times after a failed lock, want 0", stub.insertCalls)
	}
}

// TestInsertPendingIfFits_ReturnsInsertOutcome verifies the insert's answer is
// what the claim reports: no room is false without an error, and a failed
// insert surfaces its error.
func TestInsertPendingIfFits_ReturnsInsertOutcome(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("insert failed")
	for name, stub := range map[string]*claimTxStub{
		"no room":       {fits: false},
		"insert failed": {insertErr: sentinel},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fits, err := InsertPendingIfFits(context.Background(), &stubRunner{tx: stub}, claimIntent(), AcceptForWrite)
			if fits {
				t.Errorf("fits = true, want false")
			}
			if !errors.Is(err, stub.insertErr) {
				t.Errorf("err = %v, want %v", err, stub.insertErr)
			}
		})
	}
}

// -------------------------------------------------------------------------
// COPY CLAIMS
// -------------------------------------------------------------------------

// copyIntents returns n intents for one write's copies, none placed yet.
func copyIntents(n int) []*PendingObject {
	out := make([]*PendingObject, n)
	for i := range out {
		out[i] = &PendingObject{IntentID: fmt.Sprintf("i-%d", i), ObjectKey: "k", SizeBytes: 100}
	}
	return out
}

// TestClaimPendingCopies_LocksSortedThenClaimsRanked verifies every candidate
// is locked in name order before any insert, the inserts walk the ranking, and
// claiming stops once every intent has a backend.
func TestClaimPendingCopies_LocksSortedThenClaimsRanked(t *testing.T) {
	t.Parallel()
	stub := &claimTxStub{fits: true}
	claims, err := ClaimPendingCopies(context.Background(), &stubRunner{tx: stub}, copyIntents(2), []string{"b3", "b1", "b2"}, AcceptForWrite)
	if err != nil {
		t.Fatalf("ClaimPendingCopies: %v", err)
	}
	if want := []string{"b1", "b2", "b3"}; !slices.Equal(stub.locked, want) {
		t.Errorf("locked %v, want %v", stub.locked, want)
	}
	if want := []string{"backend_lock", "backend_lock", "backend_lock", "insert", "insert"}; !slices.Equal(stub.ops, want) {
		t.Errorf("ops = %v, want %v", stub.ops, want)
	}
	if len(claims.Claimed) != 2 || claims.Claimed[0].BackendName != "b3" || claims.Claimed[1].BackendName != "b1" {
		t.Errorf("claimed %+v, want the first two ranked candidates", claims.Claimed)
	}
	if stub.accepted != AcceptForWrite {
		t.Errorf("accepts = %v, want %v", stub.accepted, AcceptForWrite)
	}
}

// TestClaimPendingCopies_SkipsAFullBackend verifies a declining candidate is
// reported and passed over for the next one rather than costing a copy.
func TestClaimPendingCopies_SkipsAFullBackend(t *testing.T) {
	t.Parallel()
	stub := &claimTxStub{fits: true, full: map[string]bool{"b1": true}}
	claims, err := ClaimPendingCopies(context.Background(), &stubRunner{tx: stub}, copyIntents(2), []string{"b1", "b2", "b3"}, AcceptForWrite)
	if err != nil {
		t.Fatalf("ClaimPendingCopies: %v", err)
	}
	if !slices.Equal(claims.Declined, []string{"b1"}) {
		t.Errorf("declined %v, want [b1]", claims.Declined)
	}
	if len(claims.Claimed) != 2 || claims.Claimed[0].BackendName != "b2" || claims.Claimed[1].BackendName != "b3" {
		t.Errorf("claimed %+v, want b2 and b3", claims.Claimed)
	}
}

// TestClaimPendingCopies_ClaimsWhatFits verifies a fleet that cannot take every
// copy yields the ones it can, and one that takes none yields nothing without
// an error, leaving the caller to decide.
func TestClaimPendingCopies_ClaimsWhatFits(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		stub   *claimTxStub
		ranked []string
		want   int
	}{
		"fewer backends than copies": {&claimTxStub{fits: true}, []string{"b1"}, 1},
		"nothing fits":               {&claimTxStub{fits: false}, []string{"b1", "b2"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			claims, err := ClaimPendingCopies(context.Background(), &stubRunner{tx: tc.stub}, copyIntents(2), tc.ranked, AcceptForWrite)
			if err != nil || len(claims.Claimed) != tc.want {
				t.Errorf("ClaimPendingCopies = %d claimed, %v; want %d", len(claims.Claimed), err, tc.want)
			}
		})
	}
}

// TestClaimPendingCopies_ErrorClaimsNothing verifies a failed lock or insert
// surfaces and reports no claims, since the transaction rolls every one back.
func TestClaimPendingCopies_ErrorClaimsNothing(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("db down")
	for name, stub := range map[string]*claimTxStub{
		"lock failed":   {fits: true, lockErr: sentinel},
		"insert failed": {fits: true, insertErr: sentinel},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			claims, err := ClaimPendingCopies(context.Background(), &stubRunner{tx: stub}, copyIntents(2), []string{"b1", "b2"}, AcceptForWrite)
			if !errors.Is(err, sentinel) {
				t.Errorf("err = %v, want %v", err, sentinel)
			}
			if len(claims.Claimed) != 0 || len(claims.Declined) != 0 {
				t.Errorf("claims = %+v alongside the error, want none", claims)
			}
		})
	}
}

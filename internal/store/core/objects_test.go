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
	"slices"
	"testing"
	"time"
)

// intentClearingTxStub reports a key's pending intents to whatever clears them,
// so a commit's handling of the ones it supersedes can be asserted on.
type intentClearingTxStub struct {
	*quotaTxStub

	pending  []SupersededIntent
	clearErr error
	cleared  bool
}

func (s *intentClearingTxStub) ClearPendingForKey(_ context.Context, _ string, _ []string) ([]SupersededIntent, error) {
	s.cleared = true
	return s.pending, s.clearErr
}

// TestRecordObject_IntentClearFailureAborts verifies a write that cannot clear
// the key's intents rolls back rather than committing copies alongside intents
// that would later be resolved against the object it just replaced.
func TestRecordObject_IntentClearFailureAborts(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("clear failed")
	stub := &intentClearingTxStub{quotaTxStub: &quotaTxStub{}, clearErr: sentinel}
	_, _, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{
		Key: "k", Size: 100, Copies: []ObjectCopy{{Backend: "b1", IntentID: "mine"}},
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the clear error, got %v", err)
	}
}

// TestDeleteObject_IntentClearFailureAborts verifies the same for a delete: the
// copies and the intents go together or not at all.
func TestDeleteObject_IntentClearFailureAborts(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("clear failed")
	stub := &intentClearingTxStub{
		quotaTxStub: &quotaTxStub{existingCopies: []ExistingCopy{{BackendName: "b1", SizeBytes: 10}}},
		clearErr:    sentinel,
	}
	_, _, err := DeleteObject(context.Background(), &stubRunner{tx: stub}, "k")
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the clear error, got %v", err)
	}
	if len(stub.ops) != 0 {
		t.Errorf("quota charged despite the failure: %+v", stub.ops)
	}
}

// TestRecordObject_ClearsSupersededIntents verifies a write removes the key's
// other intents and hands back the bytes of the ones it did not land on, so
// they are cleaned off their backends instead of waiting for the reaper.
func TestRecordObject_ClearsSupersededIntents(t *testing.T) {
	t.Parallel()
	stub := &intentClearingTxStub{
		quotaTxStub: &quotaTxStub{},
		pending: []SupersededIntent{
			{IntentID: "mine", BackendName: "b1", SizeBytes: 100},
			{IntentID: "stale", BackendName: "b2", SizeBytes: 70},
		},
	}
	displaced, _, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{
		Key: "k", Size: 100, Copies: []ObjectCopy{{Backend: "b1", IntentID: "mine"}},
	})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if !stub.cleared {
		t.Fatal("expected the write to clear the key's intents")
	}
	if len(displaced) != 1 {
		t.Fatalf("expected only the stale intent's bytes, got %+v", displaced)
	}
	if displaced[0].BackendName != "b2" || displaced[0].SizeBytes != 70 {
		t.Errorf("displaced = %+v, want b2/70", displaced[0])
	}
	if displaced[0].Reason != CleanupReasonSupersededIntent {
		t.Errorf("reason = %q, want %q", displaced[0].Reason, CleanupReasonSupersededIntent)
	}
}

// TestDeleteObject_ClearsIntents verifies that deleting an object also clears
// its pending intents and hands their bytes back, since nothing is landing on
// any backend and every intent for the key is now meaningless.
func TestDeleteObject_ClearsIntents(t *testing.T) {
	t.Parallel()
	stub := &intentClearingTxStub{
		quotaTxStub: &quotaTxStub{existingCopies: []ExistingCopy{{BackendName: "b1", SizeBytes: 100}}},
		pending:     []SupersededIntent{{IntentID: "stale", BackendName: "b2", SizeBytes: 70}},
	}
	displaced, _, err := DeleteObject(context.Background(), &stubRunner{tx: stub}, "k")
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if !stub.cleared {
		t.Fatal("expected the delete to clear the key's intents")
	}
	if !slices.ContainsFunc(displaced, func(dc DeletedCopy) bool {
		return dc.BackendName == "b2" && dc.Reason == CleanupReasonSupersededIntent
	}) {
		t.Errorf("expected the stale intent's bytes handed back, got %+v", displaced)
	}
}

// multiCopyTxStub records what a commit wrote and which intents it cleared,
// which is what a write placing several copies has to get right. The embedded
// quotaTxStub keeps the stripe charges and the locked copy set.
type multiCopyTxStub struct {
	*quotaTxStub

	inserted []ObjectLocation
	cleared  []string
	kept     []string
}

// InsertObjectLocation captures each row the commit inserts.
func (s *multiCopyTxStub) InsertObjectLocation(_ context.Context, loc *ObjectLocation) error {
	s.inserted = append(s.inserted, *loc)
	return nil
}

// ClearPendingForKey captures the intents the commit resolves. A write clears
// the key's intents as a set, its own among them, rather than one at a time.
func (s *multiCopyTxStub) ClearPendingForKey(_ context.Context, objectKey string, keep []string) ([]SupersededIntent, error) {
	s.cleared = append(s.cleared, objectKey)
	s.kept = append(s.kept, keep...)
	return nil, nil
}

// newMultiCopyStub builds a commit stub over the supplied prior copy set.
func newMultiCopyStub(existing []ExistingCopy) *multiCopyTxStub {
	return &multiCopyTxStub{quotaTxStub: &quotaTxStub{existingCopies: existing}}
}

// TestRecordObject_CommitsEveryCopy verifies a write naming several backends
// inserts a row and charges bytes on each of them, and clears every intent it
// was admitted on, inside the one transaction.
func TestRecordObject_CommitsEveryCopy(t *testing.T) {
	t.Parallel()
	stub := newMultiCopyStub(nil)
	_, deltas, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{
		Key: "k", Size: 100,
		Copies: []ObjectCopy{{Backend: "b1", IntentID: "i-1"}, {Backend: "b2", IntentID: "i-2"}},
	})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if len(stub.inserted) != 2 {
		t.Fatalf("expected a row per copy, got %d", len(stub.inserted))
	}
	for i, want := range []string{"b1", "b2"} {
		if stub.inserted[i].BackendName != want || stub.inserted[i].SizeBytes != 100 {
			t.Errorf("row %d = %s/%d, want %s/100", i, stub.inserted[i].BackendName, stub.inserted[i].SizeBytes, want)
		}
	}
	for _, backend := range []string{"b1", "b2"} {
		if deltas[backend] != 100 {
			t.Errorf("delta for %s = %d, want 100", backend, deltas[backend])
		}
	}
	if len(stub.ops) != 2 {
		t.Errorf("expected a stripe charge per copy, got %+v", stub.ops)
	}
	if len(stub.cleared) != 1 || stub.cleared[0] != "k" {
		t.Errorf("expected the key's intents cleared once, got %v", stub.cleared)
	}
}

// TestRecordObject_DisplacesOnlyBackendsItLeaves verifies a multi-copy write
// hands back for cleanup only the prior copies it is not landing on. Reporting
// by a single backend would send the write's own second copy to be deleted.
func TestRecordObject_DisplacesOnlyBackendsItLeaves(t *testing.T) {
	t.Parallel()
	stub := newMultiCopyStub([]ExistingCopy{
		{BackendName: "b1", SizeBytes: 10},
		{BackendName: "b2", SizeBytes: 20},
		{BackendName: "b3", SizeBytes: 30},
	})
	displaced, _, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{
		Key: "k", Size: 100,
		Copies: []ObjectCopy{{Backend: "b1"}, {Backend: "b2"}},
	})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if len(displaced) != 1 || displaced[0].BackendName != "b3" {
		t.Errorf("expected only b3 displaced, got %+v", displaced)
	}
}

// TestRecordObject_SparesTheBackendsItIsStillPlacingOn verifies a commit does
// not displace a prior copy from a backend the same write is still uploading
// to. Those bytes sit at the path the new copy is landing on, so deleting them
// as displaced deletes the copy this write is placing and leaves the row its
// commit writes describing an object that is gone - which only a read of that
// one copy would ever notice.
func TestRecordObject_SparesTheBackendsItIsStillPlacingOn(t *testing.T) {
	t.Parallel()
	stub := newMultiCopyStub([]ExistingCopy{
		{BackendName: "b1", SizeBytes: 10},
		{BackendName: "b2", SizeBytes: 20},
		{BackendName: "b3", SizeBytes: 30},
	})
	displaced, _, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{
		Key: "k", Size: 100,
		Copies:  []ObjectCopy{{Backend: "b1", IntentID: "i-1"}},
		Placing: []ObjectCopy{{Backend: "b2", IntentID: "i-2"}},
	})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if len(displaced) != 1 || displaced[0].BackendName != "b3" {
		t.Fatalf("expected only b3 displaced, got %+v", displaced)
	}
	if !slices.Contains(stub.kept, "i-2") {
		t.Errorf("the intent of the copy still uploading was not kept: %v", stub.kept)
	}
}

// TestRecordObject_NoCopies verifies a request naming no copies is refused. It
// would otherwise clear the key's rows and put nothing back, which is a delete
// wearing a write's name.
func TestRecordObject_NoCopies(t *testing.T) {
	t.Parallel()
	stub := newMultiCopyStub([]ExistingCopy{{BackendName: "b1", SizeBytes: 10}})
	_, _, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{Key: "k", Size: 100})
	if !errors.Is(err, ErrNoCopiesToRecord) {
		t.Fatalf("expected ErrNoCopiesToRecord, got %v", err)
	}
	if len(stub.inserted) != 0 || len(stub.ops) != 0 {
		t.Errorf("transaction ran despite the empty copy set: rows=%v ops=%+v", stub.inserted, stub.ops)
	}
}

// runDeleteLocation invokes DeleteObjectLocation against the stub, discarding
// the removed byte count the callers under test do not assert on.
func runDeleteLocation(stub *excessTxStub, key, backend string) error {
	_, err := DeleteObjectLocation(context.Background(), &stubRunner{tx: stub}, key, backend)
	return err
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
	if _, _, err := DeleteObjectsBatch(context.Background(), &stubRunner{tx: stub}, []string{"k2", "k1"}); err != nil {
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
	if _, _, err := DeleteObjectsBatch(context.Background(), &stubRunner{tx: stub}, []string{"k1"}); !errors.Is(err, sentinel) {
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
	if _, _, err := DeleteObjectsBatch(context.Background(), &stubRunner{tx: stub}, []string{"k1"}); !errors.Is(err, sentinel) {
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
	if _, _, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{Key: "k", Copies: []ObjectCopy{{Backend: "b1"}}, Size: 100}); err != nil {
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
	if _, _, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{Key: "k", Copies: []ObjectCopy{{Backend: "b1"}}, Size: 100}); !errors.Is(err, sentinel) {
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
	if _, _, err := DeleteObject(context.Background(), &stubRunner{tx: stub}, "k"); err != nil {
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
	if _, _, err := DeleteObject(context.Background(), &stubRunner{tx: stub}, "k"); !errors.Is(err, sentinel) {
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
	if _, _, err := DeleteObject(context.Background(), &stubRunner{tx: stub}, "k"); !errors.Is(err, sentinel) {
		t.Errorf("expected the tag clear error, got %v", err)
	}
	if len(stub.ops) != 0 {
		t.Errorf("quota applied despite the tag clear failing: %v", stub.ops)
	}
}

// TestDeleteObjectLocation_ReportsLockedSize verifies the copy is deleted and
// the bytes reported are the ones on the locked row, not a caller-supplied
// figure: the caller debits the backend by exactly what it removed.
func TestDeleteObjectLocation_ReportsLockedSize(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b1", SizeBytes: 100},
		{BackendName: "b2", SizeBytes: 200},
	}})
	removed, err := DeleteObjectLocation(context.Background(), &stubRunner{tx: stub}, "k", "b1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.deleted) != 1 || stub.deleted[0] != "b1" {
		t.Errorf("expected b1 deleted, got %v", stub.deleted)
	}
	if removed != 100 {
		t.Errorf("removed = %d, want 100 (the locked row's size)", removed)
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

// TestDeleteObjectLocation_ReportsNothingWhenCopyGone pins the pairing the
// caller depends on: a call that deleted nothing reports zero bytes, so a
// benign no-op cannot debit a backend for a copy it still holds.
func TestDeleteObjectLocation_ReportsNothingWhenCopyGone(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b2", SizeBytes: 200},
	}})
	removed, err := DeleteObjectLocation(context.Background(), &stubRunner{tx: stub}, "k", "b1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 when the copy was already gone", removed)
	}
}

// -------------------------------------------------------------------------
// IMPORT OBJECT
// -------------------------------------------------------------------------

// TestImportOutcome_String covers the log rendering of each outcome. A passing
// run never formats one, so without this the method is dead to coverage while
// still being what an operator reads in a reconcile log line.
func TestImportOutcome_String(t *testing.T) {
	t.Parallel()
	cases := map[ImportOutcome]string{
		ImportInserted:              "inserted",
		ImportSkippedExisting:       "skipped_existing",
		ImportSkippedPendingCleanup: "skipped_pending_cleanup",
		ImportOutcome(99):           "skipped_existing",
	}
	for outcome, want := range cases {
		if got := outcome.String(); got != want {
			t.Errorf("ImportOutcome(%d).String() = %q, want %q", outcome, got, want)
		}
	}
}

// TestImportObject_SuppressedByPendingCleanup asserts a key whose delete is
// still outstanding is refused before the insert is attempted. Importing it
// would undo the delete, so the check has to come first rather than being an
// after-the-fact correction.
func TestImportObject_SuppressedByPendingCleanup(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{pendingCleanup: true}

	outcome, err := ImportObject(context.Background(), &stubRunner{tx: stub},
		&ImportObjectRequest{Key: "k", Backend: "b1", Size: 100})
	if err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	if outcome != ImportSkippedPendingCleanup {
		t.Errorf("outcome = %s, want skipped_pending_cleanup", outcome)
	}
	if len(stub.ops) != 0 {
		t.Errorf("quota was touched for a suppressed import: %+v", stub.ops)
	}
}

// TestImportObject_KeepsBackendWriteTime asserts a reported modification time
// becomes the row's CreatedAt. A discovered object was written before the
// orchestrator knew about it, so stamping the moment it was found would report
// a Last-Modified years newer than the object.
func TestImportObject_KeepsBackendWriteTime(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{}
	written := time.Date(2024, 3, 9, 8, 15, 0, 0, time.UTC)

	if _, err := ImportObject(context.Background(), &stubRunner{tx: stub},
		&ImportObjectRequest{Key: "k", Backend: "b1", Size: 100, WrittenAt: written}); err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	if stub.importedLoc == nil {
		t.Fatal("no row was inserted")
	}
	if !stub.importedLoc.CreatedAt.Equal(written) {
		t.Errorf("CreatedAt = %v, want the backend's %v", stub.importedLoc.CreatedAt, written)
	}
}

// TestImportObject_StampsWhenBackendReportsNoTime asserts a zero time becomes
// the moment of discovery rather than reaching the row as a zero timestamp.
// Not every backend reports a modification time, and an object dated to the
// zero year is worse than one dated to when it was found.
func TestImportObject_StampsWhenBackendReportsNoTime(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{}
	before := time.Now()

	if _, err := ImportObject(context.Background(), &stubRunner{tx: stub},
		&ImportObjectRequest{Key: "k", Backend: "b1", Size: 100}); err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	if stub.importedLoc == nil {
		t.Fatal("no row was inserted")
	}
	if stub.importedLoc.CreatedAt.Before(before) {
		t.Errorf("CreatedAt = %v, want a stamp at or after %v", stub.importedLoc.CreatedAt, before)
	}
}

// TestImportObject_PendingCleanupCheckError asserts a failed check aborts the
// import rather than falling through to the insert. Treating an unreadable
// cleanup queue as "nothing pending" would resurrect exactly the objects the
// check exists to protect.
func TestImportObject_PendingCleanupCheckError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("queue unreadable")
	stub := &quotaTxStub{pendingErr: sentinel}

	if _, err := ImportObject(context.Background(), &stubRunner{tx: stub},
		&ImportObjectRequest{Key: "k", Backend: "b1", Size: 100}); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the check error to abort the import", err)
	}
	if len(stub.ops) != 0 {
		t.Errorf("quota was touched after a failed check: %+v", stub.ops)
	}
}

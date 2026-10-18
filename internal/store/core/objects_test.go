// -------------------------------------------------------------------------------
// Core DeleteObjectLocation Tests
//
// Author: Alex Freidah
//
// Engine-agnostic coverage for DeleteObjectLocation: the row is deleted and the
// backend quota debited by the size read from the locked copy set, a copy that
// is gone or was replaced since the caller read it is ErrCopyChanged, and a
// quota-debit failure surfaces so the transaction rolls back. Reuses
// excessTxStub from the surplus-copy tests.
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

// TestRecordObject_ClearsSupersededIntents verifies a write clears the key's
// other intents and returns each one's bytes for cleanup, including an intent
// on a backend this write also landed on.
func TestRecordObject_ClearsSupersededIntents(t *testing.T) {
	t.Parallel()
	stub := &intentClearingTxStub{
		quotaTxStub: &quotaTxStub{},
		pending: []SupersededIntent{
			{IntentID: "older-on-b1", BackendName: "b1", StorageKey: "k!older", SizeBytes: 100},
			{IntentID: "stale", BackendName: "b2", StorageKey: "k!stale", SizeBytes: 70},
		},
	}
	displaced, _, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{
		Key: "k", Size: 100, Copies: []ObjectCopy{{Backend: "b1", IntentID: "mine", StorageKey: "k!mine"}},
	})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if !stub.cleared {
		t.Fatal("expected the write to clear the key's intents")
	}
	if len(displaced) != 2 {
		t.Fatalf("expected both cleared intents' bytes, got %+v", displaced)
	}
	byBackend := map[string]CleanupItem{}
	for _, dc := range displaced {
		byBackend[dc.BackendName] = dc
		if dc.Reason != CleanupReasonSupersededIntent {
			t.Errorf("reason = %q, want %q", dc.Reason, CleanupReasonSupersededIntent)
		}
	}
	if got := byBackend["b1"]; got.StorageKey != "k!older" || got.SizeBytes != 100 {
		t.Errorf("b1 displaced as %+v, want the older write's own path and size", got)
	}
	if got := byBackend["b2"]; got.StorageKey != "k!stale" || got.SizeBytes != 70 {
		t.Errorf("b2 displaced as %+v, want the stale intent's path and size", got)
	}
}

// overtakenTxStub is a key whose intents a newer write already cleared, so no
// claim succeeds. It records every row insert and intent deletion it sees.
type overtakenTxStub struct {
	*multiCopyTxStub

	deletedIntents []string
}

func (*overtakenTxStub) ClaimPending(context.Context, string) (bool, error) { return false, nil }

func (s *overtakenTxStub) DeletePending(_ context.Context, intentID string) error {
	s.deletedIntents = append(s.deletedIntents, intentID)
	return nil
}

// TestRecordObject_OvertakenWriteRecordsNothing verifies a write whose intent
// a newer write cleared leaves the newer object in place: no row, no charge,
// no intent clearing, and only its own bytes handed back for cleanup.
func TestRecordObject_OvertakenWriteRecordsNothing(t *testing.T) {
	t.Parallel()
	stub := &overtakenTxStub{multiCopyTxStub: newMultiCopyStub([]ExistingCopy{
		{BackendName: "b1", StorageKey: "k!newer", SizeBytes: 50},
	})}
	displaced, deltas, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{
		Key: "k", Size: 100, Copies: []ObjectCopy{{Backend: "b1", IntentID: "mine", StorageKey: "k!mine"}},
	})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if len(stub.inserted) != 0 || len(stub.ops) != 0 || len(deltas) != 0 || len(stub.cleared) != 0 {
		t.Errorf("overtaken write changed the key: rows=%v ops=%+v deltas=%v cleared=%v", stub.inserted, stub.ops, deltas, stub.cleared)
	}
	want := []CleanupItem{{BackendName: "b1", ObjectKey: "k", StorageKey: "k!mine", SizeBytes: 100, Reason: CleanupReasonSupersededIntent}}
	if !slices.Equal(displaced, want) {
		t.Errorf("displaced = %+v, want %+v", displaced, want)
	}
	if !slices.Equal(stub.deletedIntents, []string{"mine"}) {
		t.Errorf("deleted intents = %v, want [mine]", stub.deletedIntents)
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
	if !slices.ContainsFunc(displaced, func(dc CleanupItem) bool {
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

// TestRecordObject_DisplacesEveryPriorCopy verifies a multi-copy write hands
// back every prior copy for cleanup, at the path each one occupies - including
// the copies on the backends it is landing on, whose bytes are at paths this
// write does not touch.
func TestRecordObject_DisplacesEveryPriorCopy(t *testing.T) {
	t.Parallel()
	stub := newMultiCopyStub([]ExistingCopy{
		{BackendName: "b1", StorageKey: "k!old1", SizeBytes: 10},
		{BackendName: "b2", StorageKey: "k!old2", SizeBytes: 20},
		{BackendName: "b3", StorageKey: "k!old3", SizeBytes: 30},
	})
	displaced, _, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{
		Key: "k", Size: 100,
		Copies: []ObjectCopy{
			{Backend: "b1", StorageKey: "k!new1"},
			{Backend: "b2", StorageKey: "k!new2"},
		},
	})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if len(displaced) != 3 {
		t.Fatalf("expected all three prior copies displaced, got %+v", displaced)
	}
	for _, dc := range displaced {
		if dc.StorageKey == "k!new1" || dc.StorageKey == "k!new2" {
			t.Errorf("a path this write just wrote was handed to cleanup: %+v", dc)
		}
	}
}

// TestRecordObject_KeepsTheIntentsItIsStillPlacing verifies a commit leaves the
// intents of its own uploads in place while clearing every other intent for the
// key. Their bytes need no such protection - each is at its own intent's path,
// so displacing the prior copy on a backend this write is still uploading to
// cannot touch what is landing there.
func TestRecordObject_KeepsTheIntentsItIsStillPlacing(t *testing.T) {
	t.Parallel()
	stub := newMultiCopyStub([]ExistingCopy{
		{BackendName: "b1", StorageKey: "k!old1", SizeBytes: 10},
		{BackendName: "b2", StorageKey: "k!old2", SizeBytes: 20},
		{BackendName: "b3", StorageKey: "k!old3", SizeBytes: 30},
	})
	displaced, _, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{
		Key: "k", Size: 100,
		Copies:  []ObjectCopy{{Backend: "b1", IntentID: "i-1", StorageKey: "k!new1"}},
		Placing: []ObjectCopy{{Backend: "b2", IntentID: "i-2", StorageKey: "k!new2"}},
	})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if len(displaced) != 3 {
		t.Fatalf("expected all three prior copies displaced, got %+v", displaced)
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

// runDeleteLocation invokes DeleteObjectLocation against the stub for key's
// copy on backend, discarding the removed copy the callers under test do not
// assert on.
func runDeleteLocation(stub *excessTxStub, key, backend string) error {
	loc := &ObjectLocation{ObjectKey: key, BackendName: backend}
	_, err := DeleteObjectLocation(context.Background(), &stubRunner{tx: stub}, loc, 0, "")
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

// batchIntentStub is a batch whose keys each hold one in-flight intent, so a
// test can see the batch clear them the way a single delete does.
type batchIntentStub struct {
	*batchTxStub

	clearErr error
	cleared  []string
}

// ClearPendingForKey reports one stale intent per key, on b9 at the key's own
// intent path.
func (s *batchIntentStub) ClearPendingForKey(_ context.Context, key string, _ []string) ([]SupersededIntent, error) {
	if s.clearErr != nil {
		return nil, s.clearErr
	}
	s.cleared = append(s.cleared, key)
	return []SupersededIntent{{IntentID: "i-" + key, BackendName: "b9", StorageKey: key + "!inflight", SizeBytes: 5}}, nil
}

// TestDeleteObjectsBatch_ClearsIntentsAndQueues verifies the batch clears each
// removed key's intents and queues, per key in sorted order, the copies'
// bytes as deleted and the intents' bytes as superseded.
func TestDeleteObjectsBatch_ClearsIntentsAndQueues(t *testing.T) {
	t.Parallel()
	stub := &batchIntentStub{batchTxStub: newBatchStub(&batchTxStub{keyed: []KeyedExistingCopy{
		{ObjectKey: "k2", BackendName: "b1", StorageKey: "k2!a", SizeBytes: 20},
		{ObjectKey: "k1", BackendName: "b1", StorageKey: "k1!a", SizeBytes: 10},
	}})}
	cleanup, deltas, err := DeleteObjectsBatch(context.Background(), &stubRunner{tx: stub}, []string{"k2", "k1", "missing"})
	if err != nil {
		t.Fatalf("DeleteObjectsBatch: %v", err)
	}
	if !slices.Equal(stub.cleared, []string{"k1", "k2"}) {
		t.Errorf("cleared intents for %v, want each removed key in order", stub.cleared)
	}
	want := []CleanupItem{
		{BackendName: "b1", ObjectKey: "k1", StorageKey: "k1!a", Reason: CleanupReasonDeleted, SizeBytes: 10},
		{BackendName: "b9", ObjectKey: "k1", StorageKey: "k1!inflight", Reason: CleanupReasonSupersededIntent, SizeBytes: 5},
		{BackendName: "b1", ObjectKey: "k2", StorageKey: "k2!a", Reason: CleanupReasonDeleted, SizeBytes: 20},
		{BackendName: "b9", ObjectKey: "k2", StorageKey: "k2!inflight", Reason: CleanupReasonSupersededIntent, SizeBytes: 5},
	}
	if !slices.Equal(cleanup, want) {
		t.Errorf("cleanup = %+v, want %+v", cleanup, want)
	}
	if deltas["b1"] != -30 {
		t.Errorf("deltas = %+v, want b1 -30", deltas)
	}
}

// TestDeleteObjectsBatch_FailuresRollBack verifies a failed intent clear or
// queue insert surfaces with no cleanup rows handed back.
func TestDeleteObjectsBatch_FailuresRollBack(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("step failed")
	keyed := []KeyedExistingCopy{{ObjectKey: "k1", BackendName: "b1", SizeBytes: 10}}
	cases := map[string]TxAdapter{
		"intent clear": &batchIntentStub{batchTxStub: newBatchStub(&batchTxStub{keyed: keyed}), clearErr: sentinel},
		"queue":        &queueRecorder{TxAdapter: newBatchStub(&batchTxStub{keyed: keyed}), err: sentinel},
	}
	for name, tx := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cleanup, _, err := DeleteObjectsBatch(context.Background(), &stubRunner{tx: tx}, []string{"k1"})
			if !errors.Is(err, sentinel) || cleanup != nil {
				t.Errorf("cleanup=%+v err=%v, want the %s failure", cleanup, err, name)
			}
		})
	}
}

// TestDeleteObjectsBatch_NothingToDelete verifies an empty key list or keys
// that hold nothing queue nothing.
func TestDeleteObjectsBatch_NothingToDelete(t *testing.T) {
	t.Parallel()
	stub := newBatchStub(&batchTxStub{})
	for _, keys := range [][]string{nil, {"missing"}} {
		cleanup, deltas, err := DeleteObjectsBatch(context.Background(), &stubRunner{tx: stub}, keys)
		if err != nil || cleanup != nil || deltas != nil {
			t.Errorf("keys %v: cleanup=%+v deltas=%+v err=%v, want nothing", keys, cleanup, deltas, err)
		}
	}
}

// TestDeleteObject_QueuesEveryCopy verifies a delete queues each copy's bytes
// at its own path under the deleted reason, and reports a key with no copies
// as not found.
func TestDeleteObject_QueuesEveryCopy(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b1", StorageKey: "k!a", SizeBytes: 10},
		{BackendName: "b2", StorageKey: "k!b", SizeBytes: 20},
	}})
	cleanup, _, err := DeleteObject(context.Background(), &stubRunner{tx: stub}, "k")
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	want := []CleanupItem{
		{BackendName: "b1", ObjectKey: "k", StorageKey: "k!a", Reason: CleanupReasonDeleted, SizeBytes: 10},
		{BackendName: "b2", ObjectKey: "k", StorageKey: "k!b", Reason: CleanupReasonDeleted, SizeBytes: 20},
	}
	if !slices.Equal(cleanup, want) {
		t.Errorf("cleanup = %+v, want %+v", cleanup, want)
	}

	empty := newExcessStub(&excessTxStub{})
	if _, _, err := DeleteObject(context.Background(), &stubRunner{tx: empty}, "k"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("err = %v, want ErrObjectNotFound", err)
	}
}

// TestDeleteObject_FailuresRollBack verifies a failed read, row delete, stripe
// charge or queue insert surfaces with no cleanup rows handed back.
func TestDeleteObject_FailuresRollBack(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("step failed")
	existing := []ExistingCopy{{BackendName: "b1", SizeBytes: 10}}
	cases := map[string]TxAdapter{
		"read":          newExcessStub(&excessTxStub{existingErr: sentinel}),
		"row delete":    &copyDeleteFailStub{quotaTxStub: &quotaTxStub{existingCopies: existing}, err: sentinel},
		"stripe charge": newExcessStub(&excessTxStub{quotaTxStub: &quotaTxStub{adjustErr: sentinel}, existing: existing}),
		"queue":         &queueRecorder{TxAdapter: newExcessStub(&excessTxStub{existing: existing}), err: sentinel},
	}
	for name, tx := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cleanup, _, err := DeleteObject(context.Background(), &stubRunner{tx: tx}, "k")
			if !errors.Is(err, sentinel) || cleanup != nil {
				t.Errorf("cleanup=%+v err=%v, want the %s failure", cleanup, err, name)
			}
		})
	}
}

// copyDeleteFailStub fails the statement that removes a key's copies, for the
// write and delete paths that clear a whole copy set.
type copyDeleteFailStub struct {
	*quotaTxStub
	err error
}

// DeleteObjectCopies fails with the seeded error, leaving the copy set
// untouched.
func (s *copyDeleteFailStub) DeleteObjectCopies(context.Context, string) error { return s.err }

// TestRecordObject_QueuesDisplacedCopies verifies an overwrite queues every
// prior copy's bytes as overwrite-displaced alongside the superseded intents.
func TestRecordObject_QueuesDisplacedCopies(t *testing.T) {
	t.Parallel()
	stub := &intentClearingTxStub{
		quotaTxStub: &quotaTxStub{existingCopies: []ExistingCopy{{BackendName: "b1", StorageKey: "k!old", SizeBytes: 10}}},
		pending:     []SupersededIntent{{IntentID: "stale", BackendName: "b2", StorageKey: "k!stale", SizeBytes: 7}},
	}
	cleanup, _, err := RecordObject(context.Background(), &stubRunner{tx: stub}, &RecordObjectRequest{
		Key: "k", Size: 100, Copies: []ObjectCopy{{Backend: "b1", IntentID: "mine", StorageKey: "k!mine"}},
	})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	want := []CleanupItem{
		{BackendName: "b1", ObjectKey: "k", StorageKey: "k!old", Reason: CleanupReasonOverwritten, SizeBytes: 10},
		{BackendName: "b2", ObjectKey: "k", StorageKey: "k!stale", Reason: CleanupReasonSupersededIntent, SizeBytes: 7},
	}
	if !slices.Equal(cleanup, want) {
		t.Errorf("cleanup = %+v, want %+v", cleanup, want)
	}
}

// TestRecordObject_FailuresRollBack verifies each step of a commit carries its
// failure out with no cleanup rows handed back.
func TestRecordObject_FailuresRollBack(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("step failed")
	existing := []ExistingCopy{{BackendName: "b1", SizeBytes: 10}}
	cases := map[string]TxAdapter{
		"key lock":      &quotaTxStub{keyLockErr: sentinel},
		"claim":         &claimFailStub{quotaTxStub: &quotaTxStub{}, err: sentinel},
		"read":          &quotaTxStub{existingErr: sentinel},
		"copy delete":   &copyDeleteFailStub{quotaTxStub: &quotaTxStub{existingCopies: existing}, err: sentinel},
		"insert":        &insertFailStub{quotaTxStub: &quotaTxStub{}, err: sentinel},
		"stripe charge": &quotaTxStub{adjustErr: sentinel},
		"queue":         &queueRecorder{TxAdapter: &quotaTxStub{existingCopies: existing}, err: sentinel},
	}
	for name, tx := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cleanup, _, err := RecordObject(context.Background(), &stubRunner{tx: tx}, &RecordObjectRequest{
				Key: "k", Size: 100, Copies: []ObjectCopy{{Backend: "b1", IntentID: "mine"}},
			})
			if !errors.Is(err, sentinel) || cleanup != nil {
				t.Errorf("cleanup=%+v err=%v, want the %s failure", cleanup, err, name)
			}
		})
	}
}

// TestRecordObject_OvertakenFailuresRollBack verifies an overtaken write that
// cannot drop its intent or queue its bytes fails rather than committing a
// half-resolved discard.
func TestRecordObject_OvertakenFailuresRollBack(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("step failed")
	cases := map[string]TxAdapter{
		"intent delete": &overtakenDeleteFailStub{err: sentinel},
		"queue":         &queueRecorder{TxAdapter: &overtakenTxStub{multiCopyTxStub: newMultiCopyStub(nil)}, err: sentinel},
	}
	for name, tx := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := RecordObject(context.Background(), &stubRunner{tx: tx}, &RecordObjectRequest{
				Key: "k", Size: 100, Copies: []ObjectCopy{{Backend: "b1", IntentID: "mine"}},
			})
			if !errors.Is(err, sentinel) {
				t.Errorf("err = %v, want the %s failure", err, name)
			}
		})
	}
}

// claimFailStub fails every intent claim, so a write cannot tell whether it
// was overtaken.
type claimFailStub struct {
	*quotaTxStub
	err error
}

// ClaimPending fails with the seeded error rather than reporting the intent
// held or gone.
func (s *claimFailStub) ClaimPending(context.Context, string) (bool, error) { return false, s.err }

// insertFailStub fails every location insert, for the commit paths that
// record a copy.
type insertFailStub struct {
	*quotaTxStub
	err error
}

// InsertObjectLocation fails with the seeded error, so no copy is
// recorded.
func (s *insertFailStub) InsertObjectLocation(context.Context, *ObjectLocation) error { return s.err }

// overtakenDeleteFailStub is an overtaken key whose intent delete fails, so
// the discard of the losing write rolls back.
type overtakenDeleteFailStub struct {
	noopTxAdapter
	err error
}

// ClaimPending reports the intent gone, as it is for a write a newer one
// overtook.
func (*overtakenDeleteFailStub) ClaimPending(context.Context, string) (bool, error) {
	return false, nil
}

// DeletePending fails with the seeded error, so the overtaken write cannot
// drop its intent.
func (s *overtakenDeleteFailStub) DeletePending(context.Context, string) error { return s.err }

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

// TestDeleteObjectLocation_QueuesLockedSize verifies the copy is deleted and
// the bytes queued are the ones on the locked row, not a caller-supplied
// figure, under the caller's reason.
func TestDeleteObjectLocation_QueuesLockedSize(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b1", StorageKey: "k!1", SizeBytes: 100},
		{BackendName: "b2", SizeBytes: 200},
	}})
	loc := &ObjectLocation{ObjectKey: "k", BackendName: "b1", StorageKey: "k!1", SizeBytes: 1}
	removed, err := DeleteObjectLocation(context.Background(), &stubRunner{tx: stub}, loc, 0, "scrub")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.deleted) != 1 || stub.deleted[0] != "b1" {
		t.Errorf("expected b1 deleted, got %v", stub.deleted)
	}
	want := []CleanupItem{{BackendName: "b1", ObjectKey: "k", StorageKey: "k!1", Reason: "scrub", SizeBytes: 100}}
	if !slices.Equal(removed, want) {
		t.Errorf("queued = %+v, want %+v", removed, want)
	}
	if len(stub.ops) != 1 || stub.ops[0].delta != -100 {
		t.Errorf("debit = %+v, want -100 on b1", stub.ops)
	}
}

// TestDeleteObjectLocation_EmptyReasonQueuesNothing verifies a caller that
// already knows the bytes are gone removes only the row: the quota still moves
// but no cleanup row is queued.
func TestDeleteObjectLocation_EmptyReasonQueuesNothing(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{{BackendName: "b1", SizeBytes: 100}}})
	queuer := &queueRecorder{TxAdapter: stub}
	loc := &ObjectLocation{ObjectKey: "k", BackendName: "b1"}
	removed, err := DeleteObjectLocation(context.Background(), &stubRunner{tx: queuer}, loc, 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if removed != nil || len(queuer.queued) != 0 {
		t.Errorf("queued = %+v / %+v, want nothing", removed, queuer.queued)
	}
	if len(stub.deleted) != 1 || len(stub.ops) != 1 {
		t.Errorf("expected the row removed and debited, got deleted=%v ops=%v", stub.deleted, stub.ops)
	}
}

// TestDeleteObjectLocation_ChangedWhenCopyGone pins the refusal where the
// target backend no longer holds a copy: nothing is deleted and no quota moves.
func TestDeleteObjectLocation_ChangedWhenCopyGone(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b2", SizeBytes: 200},
	}})
	if err := runDeleteLocation(stub, "k", "b1"); !errors.Is(err, ErrCopyChanged) {
		t.Fatalf("expected ErrCopyChanged, got %v", err)
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

// TestDeleteObjectLocation_NoTagClearWhenCopyGone verifies the refusal path
// leaves tags alone too: the copy was already absent, so this call removed
// nothing and has no business clearing the object's tags.
func TestDeleteObjectLocation_NoTagClearWhenCopyGone(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b2", SizeBytes: 200},
	}})
	if err := runDeleteLocation(stub, "k", "b1"); !errors.Is(err, ErrCopyChanged) {
		t.Fatalf("expected ErrCopyChanged, got %v", err)
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

// TestDeleteObjectLocation_KeepsOverwrittenCopy pins the race the storage key
// guards: the caller read the copy at one path, and a client overwrite has
// since put a newer write on the same backend. That newer row must survive,
// with no quota moved and no path handed back for deletion.
func TestDeleteObjectLocation_KeepsOverwrittenCopy(t *testing.T) {
	t.Parallel()
	stub := newExcessStub(&excessTxStub{existing: []ExistingCopy{
		{BackendName: "b1", StorageKey: "k!new", SizeBytes: 200},
	}})
	loc := &ObjectLocation{ObjectKey: "k", BackendName: "b1", StorageKey: "k!old"}
	removed, err := DeleteObjectLocation(context.Background(), &stubRunner{tx: stub}, loc, 0, CleanupReasonDeleted)
	if !errors.Is(err, ErrCopyChanged) {
		t.Fatalf("expected ErrCopyChanged, got %v", err)
	}
	if removed != nil {
		t.Errorf("removed = %+v, want nothing reported", removed)
	}
	if len(stub.deleted) != 0 || len(stub.ops) != 0 || len(stub.tagsCleared) != 0 {
		t.Errorf("newer copy touched: deleted=%v ops=%v tags=%v", stub.deleted, stub.ops, stub.tagsCleared)
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

// TestImportObject_SuppressedByInFlightIntent asserts a key whose write intent
// is still in flight is skipped rather than imported as a new object.
func TestImportObject_SuppressedByInFlightIntent(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{pathInFlight: true}

	outcome, err := ImportObject(context.Background(), &stubRunner{tx: stub},
		&ImportObjectRequest{Key: "k", Backend: "b1", Size: 100})
	if err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	if outcome != ImportSkippedExisting {
		t.Errorf("outcome = %s, want skipped_existing", outcome)
	}
	if len(stub.ops) != 0 {
		t.Errorf("quota was touched for an in-flight import: %+v", stub.ops)
	}
	if stub.importedLoc != nil {
		t.Errorf("row was inserted for an in-flight import: %+v", stub.importedLoc)
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

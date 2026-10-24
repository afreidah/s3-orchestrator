// -------------------------------------------------------------------------------
// Core PlaceCopy Tests
//
// Author: Alex Freidah
//
// A placement records bytes copied from a source copy to the path its intent
// claimed, so it commits only while the intent is still held, the source is
// still the copy the caller read, and the target holds no other copy. A
// refusal still commits: it drops the intent and queues the copied bytes. A
// verbatim copy repeats every column of the source; a reformed one carries the
// intent's form and size. A move removes the source row and queues its bytes
// in the same transaction, and a rewrite moves the copy onto its own backend.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

const (
	placeOrphanReason = "test_orphan"
	placeSourceReason = "test_source_delete"
	placeIntentID     = "i-place"
	placeDestPath     = "k!dest"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// placeTxStub serves one locked source row and records what the placement
// writes, the intents it deletes, and the order of its row writes.
type placeTxStub struct {
	*quotaTxStub
	existing       []ExistingCopy
	src            *ObjectLocation
	lost           bool
	claimErr       error
	lockErr        error
	deletePendErr  error
	insertErr      error
	inserted       []*ObjectLocation
	deleted        []string
	deletedIntents []string
	steps          []string
}

// newPlaceStub builds a stub holding src as the only copy, recorded at the
// storage key src names.
func newPlaceStub(src *ObjectLocation) *placeTxStub {
	return &placeTxStub{
		quotaTxStub: &quotaTxStub{},
		existing:    []ExistingCopy{{BackendName: src.BackendName, StorageKey: src.StorageKey, SizeBytes: src.SizeBytes}},
		src:         src,
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ClaimPending reports the intent held unless the stub says a client write
// already cleared it.
func (s *placeTxStub) ClaimPending(context.Context, string) (bool, error) {
	return !s.lost, s.claimErr
}

// DeletePending records the intent the placement cleared, or fails with the
// seeded error.
func (s *placeTxStub) DeletePending(_ context.Context, intentID string) error {
	if s.deletePendErr != nil {
		return s.deletePendErr
	}
	s.deletedIntents = append(s.deletedIntents, intentID)
	return nil
}

// GetExistingCopiesForUpdate serves the seeded copy set.
func (s *placeTxStub) GetExistingCopiesForUpdate(context.Context, string) ([]ExistingCopy, error) {
	return s.existing, s.existingErr
}

// LockObjectOnBackend serves the seeded source row.
func (s *placeTxStub) LockObjectOnBackend(context.Context, string, string) (*ObjectLocation, bool, error) {
	return s.src, s.src != nil, s.lockErr
}

// InsertObjectLocation records the destination row, or fails the placement.
func (s *placeTxStub) InsertObjectLocation(_ context.Context, loc *ObjectLocation) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.inserted = append(s.inserted, loc)
	s.steps = append(s.steps, "insert "+loc.BackendName)
	return nil
}

// DeleteObjectFromBackend records the backend the copy left.
func (s *placeTxStub) DeleteObjectFromBackend(_ context.Context, _, backend string) error {
	s.deleted = append(s.deleted, backend)
	s.steps = append(s.steps, "delete "+backend)
	return nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// placedSource is an encrypted, measured copy with an identity and a write
// time, so a test can see every column ride across.
func placedSource() *ObjectLocation {
	return &ObjectLocation{
		ObjectKey:             "k",
		BackendName:           "b1",
		StorageKey:            "k!src",
		SizeBytes:             4096,
		CreatedAt:             time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Encrypted:             true,
		EncryptionKey:         []byte("dek"),
		KeyID:                 "config-0",
		PlaintextSize:         4000,
		ContentHash:           "abc",
		CompressionProbeSize:  3900,
		CompressionProbeLevel: "default",
		Identity:              &ObjectIdentity{ETag: "etag"},
	}
}

// placeIntent is the claim a placement of key k holds on backend for size
// bytes at path.
func placeIntent(backend, path string, size int64) *PendingObject {
	return &PendingObject{IntentID: placeIntentID, ObjectKey: "k", BackendName: backend, StorageKey: path, SizeBytes: size}
}

// placeReasons labels the bytes a test placement queues, so a test can tell
// the orphan rows from the source-delete rows.
func placeReasons() MoveReasons {
	return MoveReasons{Orphan: placeOrphanReason, SourceDelete: placeSourceReason}
}

// runPlaceOn runs a placement of src under intent against tx, as a move or a
// replica and with or without reformed bytes.
func runPlaceOn(tx TxAdapter, src *ObjectLocation, intent *PendingObject, move, reformed bool) (int64, []CleanupItem, error) {
	read := *src
	return stubOps(tx).PlaceCopy(context.Background(), &Placement{
		Source:       &read,
		Intent:       intent,
		RemoveSource: move,
		Reformed:     reformed,
		Reasons:      placeReasons(),
	})
}

// runPlace places the stub's source verbatim on b2 at k!dest, as a move when
// move is set.
func runPlace(stub *placeTxStub, move bool) (int64, []CleanupItem, error) {
	return runPlaceOn(stub, stub.src, placeIntent("b2", placeDestPath, stub.src.SizeBytes), move, false)
}

// orphanedIntent is the cleanup row a refused placement queues for the bytes
// it copied to b2.
func orphanedIntent(reason string) []CleanupItem {
	return []CleanupItem{{BackendName: "b2", ObjectKey: "k", StorageKey: placeDestPath, Reason: reason, SizeBytes: 4096}}
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestPlaceCopy_ReplicaCarriesEveryColumn verifies the destination row is the
// source row at the intent's address: the stored form, identity, probe and
// write time all ride across, only the target is charged, the intent is
// cleared, and nothing is queued.
func TestPlaceCopy_ReplicaCarriesEveryColumn(t *testing.T) {
	t.Parallel()
	stub := newPlaceStub(placedSource())

	placed, cleanup, err := runPlace(stub, false)
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if placed != 4096 || cleanup != nil {
		t.Errorf("placed %d bytes, cleanup %+v; want 4096 and none", placed, cleanup)
	}
	if len(stub.inserted) != 1 {
		t.Fatalf("inserted %d rows, want 1", len(stub.inserted))
	}
	want := stub.src.CopyAt("b2", placeDestPath)
	got := stub.inserted[0]
	if got.BackendName != want.BackendName || got.StorageKey != want.StorageKey ||
		!got.CreatedAt.Equal(want.CreatedAt) || got.KeyID != want.KeyID ||
		got.ContentHash != want.ContentHash || got.CompressionProbeSize != want.CompressionProbeSize ||
		!reflect.DeepEqual(got.Identity, &ObjectIdentity{ETag: "etag"}) {
		t.Errorf("inserted %+v, want the source at b2/k!dest", got)
	}
	if len(stub.deleted) != 0 {
		t.Errorf("a replica removed %v, want the source kept", stub.deleted)
	}
	if len(stub.ops) != 1 || stub.ops[0] != (quotaOp{backend: "b2", delta: 4096}) {
		t.Errorf("quota ops = %v, want b2 +4096 only", stub.ops)
	}
	if !slices.Equal(stub.deletedIntents, []string{placeIntentID}) {
		t.Errorf("deleted intents = %v, want the placement's own", stub.deletedIntents)
	}
}

// TestPlaceCopy_VerbatimIgnoresTheIntentForm verifies a placement that did not
// reform the bytes records the source's form and size, whatever the intent
// carries.
func TestPlaceCopy_VerbatimIgnoresTheIntentForm(t *testing.T) {
	t.Parallel()
	stub := newPlaceStub(placedSource())
	intent := placeIntent("b2", placeDestPath, 1)
	intent.StoredForm = *rewrittenForm()

	if _, _, err := runPlaceOn(stub, stub.src, intent, false, false); err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if got := stub.inserted[0]; got.SizeBytes != 4096 || got.KeyID != "config-0" {
		t.Errorf("inserted %+v, want the source's form at 4096 bytes", got)
	}
}

// TestPlaceCopy_MoveRemovesTheSource verifies a move deletes the source row,
// moves its bytes between the two backends, and queues the source's bytes
// under the source-delete reason in the same transaction.
func TestPlaceCopy_MoveRemovesTheSource(t *testing.T) {
	t.Parallel()
	stub := newPlaceStub(placedSource())
	queuer := &queueRecorder{TxAdapter: stub}

	_, cleanup, err := runPlaceOn(queuer, stub.src, placeIntent("b2", placeDestPath, 4096), true, false)
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if len(stub.deleted) != 1 || stub.deleted[0] != "b1" {
		t.Errorf("deleted %v, want the source on b1", stub.deleted)
	}
	got := map[string]int64{}
	for _, op := range stub.ops {
		got[op.backend] += op.delta
	}
	if got["b1"] != -4096 || got["b2"] != 4096 {
		t.Errorf("quota deltas = %v, want b1 -4096 and b2 +4096", got)
	}
	want := []CleanupItem{{ID: 1, BackendName: "b1", ObjectKey: "k", StorageKey: "k!src", Reason: placeSourceReason, SizeBytes: 4096}}
	if !slices.Equal(cleanup, want) {
		t.Errorf("cleanup = %+v, want %+v", cleanup, want)
	}
}

// TestPlaceCopy_ReplacedSourceIsChanged verifies a source a newer write
// replaced, or one that is gone, records nothing but commits the refusal: the
// intent is dropped and the bytes the caller copied are queued as orphans.
func TestPlaceCopy_ReplacedSourceIsChanged(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*placeTxStub){
		"replaced": func(s *placeTxStub) {
			s.existing = []ExistingCopy{{BackendName: "b1", StorageKey: "k!newer", SizeBytes: 10}}
		},
		"gone":                   func(s *placeTxStub) { s.existing = nil },
		"row lock finds nothing": func(s *placeTxStub) { s.src = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newPlaceStub(placedSource())
			src := *stub.src
			mutate(stub)

			_, cleanup, err := runPlaceOn(stub, &src, placeIntent("b2", placeDestPath, 4096), true, false)
			if !errors.Is(err, ErrCopyChanged) {
				t.Fatalf("error = %v, want ErrCopyChanged", err)
			}
			assertRefusalCommitted(t, stub, cleanup)
		})
	}
}

// assertRefusalCommitted checks a refused placement wrote no row and moved no
// quota, but dropped its intent and queued the copied bytes as orphans.
func assertRefusalCommitted(t *testing.T, stub *placeTxStub, cleanup []CleanupItem) {
	t.Helper()
	if len(stub.inserted) != 0 || len(stub.deleted) != 0 || len(stub.ops) != 0 {
		t.Errorf("stub mutated: inserted=%d deleted=%v ops=%v", len(stub.inserted), stub.deleted, stub.ops)
	}
	if !slices.Equal(stub.deletedIntents, []string{placeIntentID}) {
		t.Errorf("deleted intents = %v, want the refused placement's", stub.deletedIntents)
	}
	if want := orphanedIntent(placeOrphanReason); !slices.Equal(cleanup, want) {
		t.Errorf("cleanup = %+v, want %+v", cleanup, want)
	}
}

// TestPlaceCopy_TargetHoldsACopy verifies a target that already holds a copy
// is refused, and the refusal commits with the copied bytes queued.
func TestPlaceCopy_TargetHoldsACopy(t *testing.T) {
	t.Parallel()
	stub := newPlaceStub(placedSource())
	stub.existing = append(stub.existing, ExistingCopy{BackendName: "b2", StorageKey: "k!other", SizeBytes: 4096})

	_, cleanup, err := runPlace(stub, true)
	if !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("error = %v, want ErrTargetRefused", err)
	}
	assertRefusalCommitted(t, stub, cleanup)
}

// TestPlaceCopy_LostIntentQueuesItsBytes verifies a placement whose intent a
// client write already cleared records nothing, leaves the intent alone, and
// queues its bytes as a superseded intent's.
func TestPlaceCopy_LostIntentQueuesItsBytes(t *testing.T) {
	t.Parallel()
	stub := newPlaceStub(placedSource())
	stub.lost = true
	stub.existingErr = errors.New("must not read the copy set")

	_, cleanup, err := runPlace(stub, true)
	if !errors.Is(err, ErrCopyChanged) {
		t.Fatalf("error = %v, want ErrCopyChanged", err)
	}
	if len(stub.deletedIntents) != 0 || len(stub.steps) != 0 || len(stub.ops) != 0 {
		t.Errorf("stub mutated: intents=%v steps=%v ops=%v", stub.deletedIntents, stub.steps, stub.ops)
	}
	if want := orphanedIntent(CleanupReasonSupersededIntent); !slices.Equal(cleanup, want) {
		t.Errorf("cleanup = %+v, want %+v", cleanup, want)
	}
}

// TestPlaceCopy_FailuresRollBack verifies a failure at any step surfaces as
// itself, with no cleanup rows handed back, since the transaction rolls back.
func TestPlaceCopy_FailuresRollBack(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("step failed")
	cases := map[string]func(*placeTxStub){
		"key lock":       func(s *placeTxStub) { s.keyLockErr = sentinel },
		"claim":          func(s *placeTxStub) { s.claimErr = sentinel },
		"copy set read":  func(s *placeTxStub) { s.existingErr = sentinel },
		"source lock":    func(s *placeTxStub) { s.lockErr = sentinel },
		"intent delete":  func(s *placeTxStub) { s.deletePendErr = sentinel },
		"insert":         func(s *placeTxStub) { s.insertErr = sentinel },
		"stripe charge":  func(s *placeTxStub) { s.adjustErr = sentinel },
		"refusal delete": func(s *placeTxStub) { s.existing = nil; s.deletePendErr = sentinel },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newPlaceStub(placedSource())
			mutate(stub)
			placed, cleanup, err := runPlace(stub, true)
			if !errors.Is(err, sentinel) {
				t.Fatalf("error = %v, want the step failure", err)
			}
			if placed != 0 || cleanup != nil {
				t.Errorf("placed %d, cleanup %+v; want nothing on a rollback", placed, cleanup)
			}
		})
	}
}

// TestPlaceCopy_QueueFailureRollsBack verifies a cleanup row that cannot be
// queued fails the placement on every path that queues one.
func TestPlaceCopy_QueueFailureRollsBack(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("queue failed")
	cases := map[string]func(*placeTxStub){
		"move source": func(*placeTxStub) {},
		"refusal":     func(s *placeTxStub) { s.existing = nil },
		"lost intent": func(s *placeTxStub) { s.lost = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newPlaceStub(placedSource())
			mutate(stub)
			queuer := &queueRecorder{TxAdapter: stub, err: sentinel}
			if _, _, err := runPlaceOn(queuer, stub.src, placeIntent("b2", placeDestPath, 4096), true, false); !errors.Is(err, sentinel) {
				t.Fatalf("error = %v, want the queue failure", err)
			}
		})
	}
}

// rewrittenForm is the compressed and encrypted form a rewrite pass places,
// with a hash of its own so a test can see the source's hash win.
func rewrittenForm() *StoredForm {
	return &StoredForm{
		Encrypted:                true,
		EncryptionKey:            []byte("new-dek"),
		KeyID:                    "config-1",
		PlaintextSize:            1500,
		ContentHash:              "ignored",
		CompressionAlgorithm:     "zstd",
		CompressionLevel:         "default",
		CompressionFormatVersion: 1,
		LogicalSize:              4000,
	}
}

// reformedIntent is a claim on backend at k!new for bytes a rewrite reformed
// to 1600 bytes in rewrittenForm.
func reformedIntent(backend string) *PendingObject {
	intent := placeIntent(backend, "k!new", 1600)
	intent.StoredForm = *rewrittenForm()
	return intent
}

// TestPlaceCopy_InPlaceRewriteReplacesTheForm verifies a rewrite on the
// source's own backend deletes the read row before inserting the new one,
// which carries the intent's form and size, keeps the source's hash and write
// time, and drops the compression probe. The old bytes are queued.
func TestPlaceCopy_InPlaceRewriteReplacesTheForm(t *testing.T) {
	t.Parallel()
	stub := newPlaceStub(placedSource())

	placed, cleanup, err := runPlaceOn(stub, stub.src, reformedIntent("b1"), true, true)
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if placed != 1600 {
		t.Errorf("placed %d bytes, want 1600", placed)
	}
	if want := []string{"delete b1", "insert b1"}; !slices.Equal(stub.steps, want) {
		t.Fatalf("steps = %v, want %v", stub.steps, want)
	}
	got := stub.inserted[0]
	wantForm := *rewrittenForm()
	wantForm.ContentHash = "abc"
	if !reflect.DeepEqual(got.StoredForm, wantForm) {
		t.Errorf("form = %+v, want %+v", got.StoredForm, wantForm)
	}
	if got.StorageKey != "k!new" || got.SizeBytes != 1600 || !got.CreatedAt.Equal(stub.src.CreatedAt) {
		t.Errorf("inserted %+v, want b1/k!new at 1600 bytes and the source write time", got)
	}
	if got.CompressionProbeSize != 0 || got.CompressionProbeLevel != "" {
		t.Errorf("probe = %d/%q, want cleared", got.CompressionProbeSize, got.CompressionProbeLevel)
	}
	if !reflect.DeepEqual(got.Identity, stub.src.Identity) {
		t.Errorf("identity = %+v, want the source's", got.Identity)
	}
	net := map[string]int64{}
	for _, op := range stub.ops {
		net[op.backend] += op.delta
	}
	if len(net) != 1 || net["b1"] != 1600-4096 {
		t.Errorf("quota deltas = %v, want b1 moved by %d", net, 1600-4096)
	}
	if len(cleanup) != 1 || cleanup[0].StorageKey != "k!src" || cleanup[0].Reason != placeSourceReason {
		t.Errorf("cleanup = %+v, want the old bytes at k!src", cleanup)
	}
}

// TestPlaceCopy_InPlaceWithoutRemoveIsRefused verifies a placement onto the
// source's own backend that keeps the source is refused, since a key holds one
// copy per backend, and the refusal queues the copied bytes.
func TestPlaceCopy_InPlaceWithoutRemoveIsRefused(t *testing.T) {
	t.Parallel()
	stub := newPlaceStub(placedSource())

	_, cleanup, err := runPlaceOn(stub, stub.src, reformedIntent("b1"), false, true)
	if !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("error = %v, want ErrTargetRefused", err)
	}
	if len(stub.steps) != 0 || len(stub.ops) != 0 {
		t.Errorf("stub touched: steps=%v ops=%v", stub.steps, stub.ops)
	}
	want := []CleanupItem{{BackendName: "b1", ObjectKey: "k", StorageKey: "k!new", Reason: placeOrphanReason, SizeBytes: 1600}}
	if !slices.Equal(cleanup, want) {
		t.Errorf("cleanup = %+v, want %+v", cleanup, want)
	}
}

// TestPlaceCopy_FormOnAnotherBackend verifies a reformed copy placed on a
// different backend is still refused when that backend holds a copy, and
// otherwise carries the new form at its new size.
func TestPlaceCopy_FormOnAnotherBackend(t *testing.T) {
	t.Parallel()
	holds := newPlaceStub(placedSource())
	holds.existing = append(holds.existing, ExistingCopy{BackendName: "b2", StorageKey: "k!other", SizeBytes: 4096})
	if _, _, err := runPlaceOn(holds, holds.src, reformedIntent("b2"), true, true); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("error = %v, want ErrTargetRefused", err)
	}

	stub := newPlaceStub(placedSource())
	if _, _, err := runPlaceOn(stub, stub.src, reformedIntent("b2"), true, true); err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if got := stub.inserted[0]; got.SizeBytes != 1600 || got.KeyID != "config-1" || got.BackendName != "b2" {
		t.Errorf("inserted %+v, want the new form on b2 at 1600 bytes", got)
	}
}

// TestPlacementDeclined verifies the two declines are told apart from a
// failure, which callers count as an error rather than a skip.
func TestPlacementDeclined(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]bool{
		ErrCopyChanged:               true,
		ErrTargetRefused:             true,
		errors.New("db unreachable"): false,
		nil:                          false,
	} {
		if got := PlacementDeclined(err); got != want {
			t.Errorf("PlacementDeclined(%v) = %v, want %v", err, got, want)
		}
	}
}

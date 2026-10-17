// -------------------------------------------------------------------------------
// Core PlaceCopy Tests
//
// Author: Alex Freidah
//
// A placement records bytes copied verbatim from a source copy, so it commits
// only while that source is still the copy the caller read, the target holds
// no copy and admits the bytes. The destination row repeats every column of
// the source, including the write time and the compression probe, and a move
// removes the source row in the same transaction.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// placeTxStub serves one locked source row and records what the placement
// writes.
type placeTxStub struct {
	*quotaTxStub
	existing  []ExistingCopy
	src       *ObjectLocation
	refuses   bool
	insertErr error
	inserted  []*ObjectLocation
	deleted   []string
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

// GetExistingCopiesForUpdate serves the seeded copy set.
func (s *placeTxStub) GetExistingCopiesForUpdate(context.Context, string) ([]ExistingCopy, error) {
	return s.existing, nil
}

// LockObjectOnBackend serves the seeded source row.
func (s *placeTxStub) LockObjectOnBackend(context.Context, string, string) (*ObjectLocation, bool, error) {
	return s.src, s.src != nil, nil
}

// BackendAdmits refuses every write when the stub is set to.
func (s *placeTxStub) BackendAdmits(context.Context, string, int64) (bool, error) {
	return !s.refuses, nil
}

// InsertObjectLocation records the destination row, or fails the placement.
func (s *placeTxStub) InsertObjectLocation(_ context.Context, loc *ObjectLocation) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.inserted = append(s.inserted, loc)
	return nil
}

// DeleteObjectFromBackend records the backend the copy left.
func (s *placeTxStub) DeleteObjectFromBackend(_ context.Context, _, backend string) error {
	s.deleted = append(s.deleted, backend)
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

// runPlace places the stub's source on b2 at k!dest.
func runPlace(stub *placeTxStub, move bool) (int64, error) {
	read := *stub.src
	return PlaceCopy(context.Background(), &stubRunner{tx: stub}, &Placement{
		Source:       &read,
		Target:       "b2",
		StorageKey:   "k!dest",
		RemoveSource: move,
	})
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestPlaceCopy_ReplicaCarriesEveryColumn verifies the destination row is the
// source row at a new address: the stored form, identity, probe and write time
// all ride across, and only the target is charged.
func TestPlaceCopy_ReplicaCarriesEveryColumn(t *testing.T) {
	t.Parallel()
	stub := newPlaceStub(placedSource())

	placed, err := runPlace(stub, false)
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if placed != 4096 {
		t.Errorf("placed %d bytes, want 4096", placed)
	}
	if len(stub.inserted) != 1 {
		t.Fatalf("inserted %d rows, want 1", len(stub.inserted))
	}
	want := stub.src.CopyAt("b2", "k!dest")
	got := stub.inserted[0]
	if got.BackendName != want.BackendName || got.StorageKey != want.StorageKey ||
		!got.CreatedAt.Equal(want.CreatedAt) || got.KeyID != want.KeyID ||
		got.ContentHash != want.ContentHash || got.CompressionProbeSize != want.CompressionProbeSize ||
		got.RecordedETag() != "etag" {
		t.Errorf("inserted %+v, want the source at b2/k!dest", got)
	}
	if len(stub.deleted) != 0 {
		t.Errorf("a replica removed %v, want the source kept", stub.deleted)
	}
	if len(stub.ops) != 1 || stub.ops[0] != (quotaOp{backend: "b2", delta: 4096}) {
		t.Errorf("quota ops = %v, want b2 +4096 only", stub.ops)
	}
}

// TestPlaceCopy_MoveRemovesTheSource verifies a move deletes the source row
// and moves its bytes between the two backends in the same transaction.
func TestPlaceCopy_MoveRemovesTheSource(t *testing.T) {
	t.Parallel()
	stub := newPlaceStub(placedSource())

	if _, err := runPlace(stub, true); err != nil {
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
}

// TestPlaceCopy_ReplacedSourceIsChanged verifies a source a newer write
// replaced, or one that is gone, records nothing: the bytes the caller copied
// are the old object's.
func TestPlaceCopy_ReplacedSourceIsChanged(t *testing.T) {
	t.Parallel()
	cases := map[string][]ExistingCopy{
		"replaced": {{BackendName: "b1", StorageKey: "k!newer", SizeBytes: 10}},
		"gone":     nil,
	}
	for name, existing := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newPlaceStub(placedSource())
			stub.existing = existing

			_, err := runPlace(stub, true)
			if !errors.Is(err, ErrCopyChanged) {
				t.Fatalf("error = %v, want ErrCopyChanged", err)
			}
			if len(stub.inserted) != 0 || len(stub.deleted) != 0 || len(stub.ops) != 0 {
				t.Errorf("stub mutated: inserted=%d deleted=%v ops=%v", len(stub.inserted), stub.deleted, stub.ops)
			}
		})
	}
}

// TestPlaceCopy_TargetRefuses verifies a target that already holds a copy, or
// does not admit the bytes, records nothing.
func TestPlaceCopy_TargetRefuses(t *testing.T) {
	t.Parallel()
	holds := newPlaceStub(placedSource())
	holds.existing = append(holds.existing, ExistingCopy{BackendName: "b2", StorageKey: "k!other", SizeBytes: 4096})
	full := newPlaceStub(placedSource())
	full.refuses = true

	for name, stub := range map[string]*placeTxStub{"holds a copy": holds, "refuses admission": full} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := runPlace(stub, true)
			if !errors.Is(err, ErrTargetRefused) {
				t.Fatalf("error = %v, want ErrTargetRefused", err)
			}
			if len(stub.inserted) != 0 || len(stub.deleted) != 0 || len(stub.ops) != 0 {
				t.Errorf("stub mutated: inserted=%d deleted=%v ops=%v", len(stub.inserted), stub.deleted, stub.ops)
			}
		})
	}
}

// TestPlaceCopy_InsertFailureAbortsBeforeTheSourceGoes verifies a failed
// insert surfaces and leaves the source row and the quotas untouched, so the
// transaction rolls back with nothing half-moved.
func TestPlaceCopy_InsertFailureAbortsBeforeTheSourceGoes(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("insert failed")
	stub := newPlaceStub(placedSource())
	stub.insertErr = sentinel

	if _, err := runPlace(stub, true); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the insert failure", err)
	}
	if len(stub.deleted) != 0 || len(stub.ops) != 0 {
		t.Errorf("stub mutated: deleted=%v ops=%v", stub.deleted, stub.ops)
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

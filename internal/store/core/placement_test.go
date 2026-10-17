// -------------------------------------------------------------------------------
// Core PlaceCopy Tests
//
// Author: Alex Freidah
//
// A placement records bytes copied from a source copy, so it commits only
// while that source is still the copy the caller read, the target holds no
// other copy and admits the bytes. A verbatim copy repeats every column of the
// source, including the write time and the compression probe; a transformed
// one carries its new form and size. A move removes the source row in the same
// transaction, and a rewrite moves the copy onto its own backend.
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

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// placeTxStub serves one locked source row and records what the placement
// writes, the admission it asked for, and the order of its row writes.
type placeTxStub struct {
	*quotaTxStub
	existing  []ExistingCopy
	src       *ObjectLocation
	refuses   bool
	admitErr  error
	insertErr error
	inserted  []*ObjectLocation
	deleted   []string
	admits    []admitCall
	steps     []string
}

// admitCall is one recorded BackendAdmits question.
type admitCall struct {
	backend string
	size    int64
	accepts BackendFlags
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

// BackendAdmits records the question and refuses every write when the stub is
// set to.
func (s *placeTxStub) BackendAdmits(_ context.Context, backend string, size int64, accepts BackendFlags) (bool, error) {
	s.admits = append(s.admits, admitCall{backend: backend, size: size, accepts: accepts})
	return !s.refuses, s.admitErr
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
		!reflect.DeepEqual(got.Identity, &ObjectIdentity{ETag: "etag"}) {
		t.Errorf("inserted %+v, want the source at b2/k!dest", got)
	}
	if len(stub.deleted) != 0 {
		t.Errorf("a replica removed %v, want the source kept", stub.deleted)
	}
	if len(stub.ops) != 1 || stub.ops[0] != (quotaOp{backend: "b2", delta: 4096}) {
		t.Errorf("quota ops = %v, want b2 +4096 only", stub.ops)
	}
	if want := []admitCall{{backend: "b2", size: 4096, accepts: AcceptForWrite}}; !slices.Equal(stub.admits, want) {
		t.Errorf("admits = %+v, want %+v", stub.admits, want)
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

// TestPlaceCopy_InsertFailureChargesNothing verifies a failed insert surfaces
// before any quota moves, so the transaction rolls back the source delete with
// nothing charged.
func TestPlaceCopy_InsertFailureChargesNothing(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("insert failed")
	stub := newPlaceStub(placedSource())
	stub.insertErr = sentinel

	if _, err := runPlace(stub, true); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the insert failure", err)
	}
	if len(stub.ops) != 0 {
		t.Errorf("quota ops = %v, want none", stub.ops)
	}
}

// TestPlaceCopy_AdmissionErrorSurfaces verifies a failed admission read
// surfaces as itself, not as a refusal, and writes nothing.
func TestPlaceCopy_AdmissionErrorSurfaces(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("capacity read failed")
	stub := newPlaceStub(placedSource())
	stub.admitErr = sentinel

	if _, err := runPlace(stub, true); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the admission failure", err)
	}
	if len(stub.steps) != 0 || len(stub.ops) != 0 {
		t.Errorf("stub mutated: steps=%v ops=%v", stub.steps, stub.ops)
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

// runRewrite places the stub's source back on its own backend at k!new with
// form and size.
func runRewrite(stub *placeTxStub, form *StoredForm, size int64, move bool) (int64, error) {
	read := *stub.src
	return PlaceCopy(context.Background(), &stubRunner{tx: stub}, &Placement{
		Source:       &read,
		Target:       read.BackendName,
		StorageKey:   "k!new",
		RemoveSource: move,
		Form:         form,
		Size:         size,
	})
}

// TestPlaceCopy_InPlaceRewriteReplacesTheForm verifies a rewrite on the
// source's own backend deletes the read row before inserting the new one,
// which carries the new form and size, keeps the source's hash and write
// time, and drops the compression probe.
func TestPlaceCopy_InPlaceRewriteReplacesTheForm(t *testing.T) {
	t.Parallel()
	stub := newPlaceStub(placedSource())

	placed, err := runRewrite(stub, rewrittenForm(), 1600, true)
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
}

// TestPlaceCopy_InPlaceAdmitsOnlyTheGrowth verifies a rewrite asks the backend
// for the bytes it adds, nothing for one that shrinks, and accepts a cordoned
// backend, while a move to another backend asks for every byte in full
// service.
func TestPlaceCopy_InPlaceAdmitsOnlyTheGrowth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		size int64
		want admitCall
	}{
		{"grows", 5000, admitCall{backend: "b1", size: 5000 - 4096, accepts: AcceptForRewrite}},
		{"shrinks", 1000, admitCall{backend: "b1", size: 0, accepts: AcceptForRewrite}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := newPlaceStub(placedSource())
			if _, err := runRewrite(stub, rewrittenForm(), tc.size, true); err != nil {
				t.Fatalf("PlaceCopy: %v", err)
			}
			if !slices.Equal(stub.admits, []admitCall{tc.want}) {
				t.Errorf("admits = %+v, want %+v", stub.admits, tc.want)
			}
		})
	}

	t.Run("refused growth records nothing", func(t *testing.T) {
		t.Parallel()
		stub := newPlaceStub(placedSource())
		stub.refuses = true
		if _, err := runRewrite(stub, rewrittenForm(), 5000, true); !errors.Is(err, ErrTargetRefused) {
			t.Fatalf("error = %v, want ErrTargetRefused", err)
		}
		if len(stub.steps) != 0 || len(stub.ops) != 0 {
			t.Errorf("stub mutated: steps=%v ops=%v", stub.steps, stub.ops)
		}
	})
}

// TestPlaceCopy_InPlaceWithoutRemoveIsRefused verifies a placement onto the
// source's own backend that keeps the source is refused before the key is
// even locked, since a key holds one copy per backend.
func TestPlaceCopy_InPlaceWithoutRemoveIsRefused(t *testing.T) {
	t.Parallel()
	stub := newPlaceStub(placedSource())
	stub.keyLockErr = errors.New("must not lock")

	if _, err := runRewrite(stub, rewrittenForm(), 1600, false); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("error = %v, want ErrTargetRefused", err)
	}
	if len(stub.steps) != 0 || len(stub.admits) != 0 {
		t.Errorf("stub touched: steps=%v admits=%v", stub.steps, stub.admits)
	}
}

// TestPlaceCopy_FormOnAnotherBackend verifies a transformed copy placed on a
// different backend is still refused when that backend holds a copy, and
// otherwise carries the new form at its full size.
func TestPlaceCopy_FormOnAnotherBackend(t *testing.T) {
	t.Parallel()
	place := func(stub *placeTxStub) (int64, error) {
		read := *stub.src
		return PlaceCopy(context.Background(), &stubRunner{tx: stub}, &Placement{
			Source: &read, Target: "b2", StorageKey: "k!dest", RemoveSource: true,
			Form: rewrittenForm(), Size: 1600,
		})
	}

	holds := newPlaceStub(placedSource())
	holds.existing = append(holds.existing, ExistingCopy{BackendName: "b2", StorageKey: "k!other", SizeBytes: 4096})
	if _, err := place(holds); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("error = %v, want ErrTargetRefused", err)
	}

	stub := newPlaceStub(placedSource())
	if _, err := place(stub); err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if want := []admitCall{{backend: "b2", size: 1600, accepts: AcceptForWrite}}; !slices.Equal(stub.admits, want) {
		t.Errorf("admits = %+v, want %+v", stub.admits, want)
	}
	if got := stub.inserted[0]; got.SizeBytes != 1600 || got.KeyID != "config-1" {
		t.Errorf("inserted %+v, want the new form at 1600 bytes", got)
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

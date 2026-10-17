// -------------------------------------------------------------------------------
// SQLite Store - Backend Filter Tests
//
// Author: Alex Freidah
//
// Covers the backend filter the maintenance listings take: naming one selects
// only its copies, and an empty name selects every backend, which is what a
// fleet-wide pass asks for.
//
// Asserted against the query rather than the caller on purpose. Filtering after
// a page is read would spend the row limit on copies the pass then discards, so
// the point being pinned is that the rows never come back at all.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestBackendFilter_CopiesByForm asserts the copy listing every bulk pass walks
// selects one backend's copies when named and every backend when not, whatever
// form filter it is combined with.
func TestBackendFilter_CopiesByForm(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/b", "backend-b", 200)

	for _, f := range []core.FormFilter{{Without: core.StoredEncrypted}, backfillCopies} {
		if all := listByForm(t, s, &f); len(all) != 2 {
			t.Fatalf("fleet-wide %+v = %d rows, want 2", f, len(all))
		}
		f.Backend = "backend-b"
		if scoped := listByForm(t, s, &f); len(scoped) != 1 || scoped[0].BackendName != "backend-b" {
			t.Errorf("scoped %+v = %+v, want one row on backend-b", f, scoped)
		}
	}
}

// TestBackendFilter_UnknownBackendSelectsNothing asserts a name no copy carries
// returns an empty page rather than every row, which is what would happen if
// the filter were dropped from the predicate.
func TestBackendFilter_UnknownBackendSelectsNothing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)

	if rows := listByForm(t, s, &core.FormFilter{Without: core.StoredEncrypted, Backend: "backend-absent"}); len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}
}

// encryptInPlace places loc back on its own backend as an encrypted copy, the
// way an encryption pass records the bytes it rewrote.
func encryptInPlace(s *Store, loc *core.ObjectLocation) (int64, error) {
	return s.PlaceCopy(context.Background(), &core.Placement{
		Source:       loc,
		Target:       loc.BackendName,
		StorageKey:   loc.StorageKey + "!enc",
		RemoveSource: true,
		Form:         &core.StoredForm{Encrypted: true, EncryptionKey: []byte("k"), KeyID: "test-key", PlaintextSize: loc.SizeBytes},
		Size:         loc.SizeBytes + 100,
	})
}

// TestConversionRace_RefusesAChangedCopy pins that a rewrite placed against a
// copy a client replaced since the pass read it is ErrCopyChanged, and leaves
// the newer write's row as it was.
func TestConversionRace_RefusesAChangedCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	key := "bucket/raced"
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key:      key,
		Copies:   []core.ObjectCopy{{Backend: "backend-a", StorageKey: key + "!w1"}},
		Size:     100,
		Identity: &core.ObjectIdentity{ETag: "etag-v1"},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	read := storetest.LocationOn(t, s, key, "backend-a")
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key:      key,
		Copies:   []core.ObjectCopy{{Backend: "backend-a", StorageKey: key + "!w2"}},
		Size:     140,
		Identity: &core.ObjectIdentity{ETag: "etag-v2"},
	}); err != nil {
		t.Fatalf("RecordObject (client write): %v", err)
	}

	if _, err := encryptInPlace(s, read); !errors.Is(err, core.ErrCopyChanged) {
		t.Fatalf("PlaceCopy = %v, want ErrCopyChanged", err)
	}

	got := storetest.LocationOn(t, s, key, "backend-a")
	if got.Encrypted || got.StorageKey != key+"!w2" || got.SizeBytes != 140 {
		t.Errorf("row = %+v, want the client's write untouched", got)
	}
}

// TestConversionRace_ConvertsAnUnchangedCopy is the control: a rewrite of an
// untouched copy commits at its new path.
func TestConversionRace_ConvertsAnUnchangedCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	key := "bucket/quiet"
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key:      key,
		Copies:   []core.ObjectCopy{{Backend: "backend-a", StorageKey: key + "!w1"}},
		Size:     100,
		Identity: &core.ObjectIdentity{ETag: "etag-v1"},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	if _, err := encryptInPlace(s, storetest.LocationOn(t, s, key, "backend-a")); err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	got := storetest.LocationOn(t, s, key, "backend-a")
	if !got.Encrypted || got.StorageKey != key+"!w1!enc" || got.SizeBytes != 200 {
		t.Errorf("row = %+v, want the encrypted copy at its new path", got)
	}
}

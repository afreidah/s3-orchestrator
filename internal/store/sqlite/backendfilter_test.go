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

// TestConversionRace_RefusesAChangedCopy pins the etag predicate on the SQLite
// engine, where the null-safe comparison is IS rather than IS NOT DISTINCT
// FROM. A copy a client replaced must not be stamped with the form the pass
// read before it.
func TestConversionRace_RefusesAChangedCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	key := "bucket/raced"
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key:      key,
		Copies:   []core.ObjectCopy{{Backend: "backend-a"}},
		Size:     100,
		Identity: &core.ObjectIdentity{ETag: "etag-v1"},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key:      key,
		Copies:   []core.ObjectCopy{{Backend: "backend-a"}},
		Size:     140,
		Identity: &core.ObjectIdentity{ETag: "etag-v2"},
	}); err != nil {
		t.Fatalf("RecordObject (client write): %v", err)
	}

	err := s.MarkObjectEncrypted(ctx, &core.EncryptedUpdate{
		ObjectKey:      key,
		BackendName:    "backend-a",
		EncryptionKey:  []byte("k"),
		KeyID:          "test-key",
		PlaintextSize:  100,
		CiphertextSize: 200,
		ExpectedEtag:   "etag-v1",
	})
	if !errors.Is(err, core.ErrCopyChanged) {
		t.Fatalf("MarkObjectEncrypted = %v, want ErrCopyChanged", err)
	}

	locs, err := s.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if locs[0].Encrypted {
		t.Error("the row was stamped encrypted despite the copy having changed")
	}
}

// TestConversionRace_ConvertsAnUnchangedCopy is the control: the predicate has
// to let an untouched copy through.
func TestConversionRace_ConvertsAnUnchangedCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	key := "bucket/quiet"
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key:      key,
		Copies:   []core.ObjectCopy{{Backend: "backend-a"}},
		Size:     100,
		Identity: &core.ObjectIdentity{ETag: "etag-v1"},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	if err := s.MarkObjectEncrypted(ctx, &core.EncryptedUpdate{
		ObjectKey:      key,
		BackendName:    "backend-a",
		EncryptionKey:  []byte("k"),
		KeyID:          "test-key",
		PlaintextSize:  100,
		CiphertextSize: 200,
		ExpectedEtag:   "etag-v1",
	}); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}
}

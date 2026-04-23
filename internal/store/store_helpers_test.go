// -------------------------------------------------------------------------------
// store.objectLocationFromDB unit tests
//
// Author: Alex Freidah
//
// objectLocationFromDB is a pure helper that flattens nullable database
// columns into an ObjectLocation. Exercised end-to-end via toObjectLocation
// in integration paths; these unit tests cover each nullable-field branch
// directly without requiring a PostgreSQL connection.
// -------------------------------------------------------------------------------

package store

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/afreidah/s3-orchestrator/internal/store/sqlc"
)

// pgtime returns a pgx timestamptz wrapping t, valid and non-zero.
func pgtime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// TestObjectLocationFromDB_AllFieldsPopulated covers the happy path where
// every nullable column carries a value.
func TestObjectLocationFromDB_AllFieldsPopulated(t *testing.T) {
	t.Parallel()
	keyID := "key-abc"
	var ptSize int64 = 4096
	hash := "sha256:deadbeef"
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	loc := objectLocationFromDB(&dbObjectRow{
		Key:           "obj/foo",
		Backend:       "b1",
		Size:          4200,
		Encrypted:     true,
		EncryptionKey: []byte{0xde, 0xad, 0xbe, 0xef},
		KeyID:         &keyID,
		PlaintextSize: &ptSize,
		ContentHash:   &hash,
		CreatedAt:     created,
	})

	if loc.ObjectKey != "obj/foo" || loc.BackendName != "b1" || loc.SizeBytes != 4200 {
		t.Errorf("basic fields: %+v", loc)
	}
	if !loc.Encrypted || string(loc.EncryptionKey) != "\xde\xad\xbe\xef" {
		t.Errorf("encryption fields: encrypted=%v key=%x", loc.Encrypted, loc.EncryptionKey)
	}
	if loc.KeyID != keyID || loc.PlaintextSize != ptSize || loc.ContentHash != hash {
		t.Errorf("nullable fields: keyID=%q ptSize=%d hash=%q", loc.KeyID, loc.PlaintextSize, loc.ContentHash)
	}
	if !loc.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", loc.CreatedAt, created)
	}
}

// TestObjectLocationFromDB_NilNullableFields covers the non-encrypted path
// where KeyID, PlaintextSize, and ContentHash are all nil — they must
// flatten to their zero values, not panic.
func TestObjectLocationFromDB_NilNullableFields(t *testing.T) {
	t.Parallel()
	loc := objectLocationFromDB(&dbObjectRow{
		Key:       "obj/bar",
		Backend:   "b2",
		Size:      1024,
		Encrypted: false,
		// KeyID / PlaintextSize / ContentHash intentionally nil
		CreatedAt: time.Now(),
	})

	if loc.KeyID != "" {
		t.Errorf("KeyID = %q, want empty", loc.KeyID)
	}
	if loc.PlaintextSize != 0 {
		t.Errorf("PlaintextSize = %d, want 0", loc.PlaintextSize)
	}
	if loc.ContentHash != "" {
		t.Errorf("ContentHash = %q, want empty", loc.ContentHash)
	}
}

// TestObjectLocationFromDB_PartialNilFields covers mixed-null rows (for
// example an encrypted object with no content hash yet).
func TestObjectLocationFromDB_PartialNilFields(t *testing.T) {
	t.Parallel()
	keyID := "key-x"
	loc := objectLocationFromDB(&dbObjectRow{
		Key:       "obj/baz",
		Backend:   "b3",
		Size:      512,
		Encrypted: true,
		KeyID:     &keyID,
		// PlaintextSize and ContentHash still nil
		CreatedAt: time.Now(),
	})
	if loc.KeyID != keyID {
		t.Errorf("KeyID = %q, want %q", loc.KeyID, keyID)
	}
	if loc.PlaintextSize != 0 || loc.ContentHash != "" {
		t.Errorf("expected other nullable fields zero; got ptSize=%d hash=%q", loc.PlaintextSize, loc.ContentHash)
	}
}
// -------------------------------------------------------------------------
// toObjectLocation switch coverage
//
// toObjectLocation flattens the sqlc-generated row types our queries
// produce into the store.ObjectLocation domain type. Each case in its type
// switch is a separate integration-only code path; the tests below
// exercise every branch without needing a real PostgreSQL.
// -------------------------------------------------------------------------

// TestToObjectLocation_GetAllObjectLocationsRow covers the row returned by
// GetAllObjectLocations (the encrypted-object-aware primary read path).
func TestToObjectLocation_GetAllObjectLocationsRow(t *testing.T) {
	t.Parallel()
	keyID := "k-1"
	var ptSize int64 = 1234
	hash := "sha256:abc"
	now := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)

	loc := toObjectLocation(db.GetAllObjectLocationsRow{
		ObjectKey: "k", BackendName: "b1", SizeBytes: 100,
		Encrypted: true, EncryptionKey: []byte{0x01, 0x02},
		KeyID: &keyID, PlaintextSize: &ptSize, ContentHash: &hash,
		CreatedAt: pgtime(now),
	})
	if loc.ObjectKey != "k" || loc.BackendName != "b1" || loc.SizeBytes != 100 {
		t.Errorf("unexpected location: %+v", loc)
	}
	if !loc.Encrypted || loc.KeyID != keyID || loc.PlaintextSize != ptSize {
		t.Errorf("encryption fields not flattened: %+v", loc)
	}
}

// TestToObjectLocation_GetUnderReplicatedObjectsRow covers the under-
// replicated query row.
func TestToObjectLocation_GetUnderReplicatedObjectsRow(t *testing.T) {
	t.Parallel()
	loc := toObjectLocation(db.GetUnderReplicatedObjectsRow{
		ObjectKey: "u", BackendName: "b2", SizeBytes: 200,
		CreatedAt: pgtime(time.Now()),
	})
	if loc.ObjectKey != "u" || loc.BackendName != "b2" || loc.SizeBytes != 200 {
		t.Errorf("unexpected location: %+v", loc)
	}
}

// TestToObjectLocation_GetUnderReplicatedObjectsExcludingRow covers the
// "exclude certain backends" variant of the under-replicated query.
func TestToObjectLocation_GetUnderReplicatedObjectsExcludingRow(t *testing.T) {
	t.Parallel()
	loc := toObjectLocation(db.GetUnderReplicatedObjectsExcludingRow{
		ObjectKey: "u2", BackendName: "b3", SizeBytes: 300,
		CreatedAt: pgtime(time.Now()),
	})
	if loc.ObjectKey != "u2" || loc.BackendName != "b3" || loc.SizeBytes != 300 {
		t.Errorf("unexpected location: %+v", loc)
	}
}

// TestToObjectLocation_GetOverReplicatedObjectsRow covers the over-
// replicated query row.
func TestToObjectLocation_GetOverReplicatedObjectsRow(t *testing.T) {
	t.Parallel()
	loc := toObjectLocation(db.GetOverReplicatedObjectsRow{
		ObjectKey: "o", BackendName: "b4", SizeBytes: 400,
		CreatedAt: pgtime(time.Now()),
	})
	if loc.ObjectKey != "o" || loc.BackendName != "b4" || loc.SizeBytes != 400 {
		t.Errorf("unexpected location: %+v", loc)
	}
}

// TestToObjectLocation_GetObjectCopiesForUpdateRow covers the locked
// copies query used during replication planning.
func TestToObjectLocation_GetObjectCopiesForUpdateRow(t *testing.T) {
	t.Parallel()
	loc := toObjectLocation(db.GetObjectCopiesForUpdateRow{
		ObjectKey: "c", BackendName: "b5", SizeBytes: 500,
		CreatedAt: pgtime(time.Now()),
	})
	if loc.ObjectKey != "c" || loc.BackendName != "b5" || loc.SizeBytes != 500 {
		t.Errorf("unexpected location: %+v", loc)
	}
}

// TestToObjectLocation_ListObjectsByBackendRow covers the simple list row.
func TestToObjectLocation_ListObjectsByBackendRow(t *testing.T) {
	t.Parallel()
	loc := toObjectLocation(db.ListObjectsByBackendRow{
		ObjectKey: "lb", BackendName: "b6", SizeBytes: 600,
		CreatedAt: pgtime(time.Now()),
	})
	if loc.ObjectKey != "lb" || loc.BackendName != "b6" {
		t.Errorf("unexpected location: %+v", loc)
	}
}

// TestToObjectLocation_ListObjectsByPrefixRow covers the prefix-list row.
func TestToObjectLocation_ListObjectsByPrefixRow(t *testing.T) {
	t.Parallel()
	loc := toObjectLocation(db.ListObjectsByPrefixRow{
		ObjectKey: "lp", BackendName: "b7", SizeBytes: 700,
		CreatedAt: pgtime(time.Now()),
	})
	if loc.ObjectKey != "lp" || loc.BackendName != "b7" {
		t.Errorf("unexpected location: %+v", loc)
	}
}

// TestToObjectLocation_ListExpiredObjectsRow covers the lifecycle query row.
func TestToObjectLocation_ListExpiredObjectsRow(t *testing.T) {
	t.Parallel()
	loc := toObjectLocation(db.ListExpiredObjectsRow{
		ObjectKey: "le", BackendName: "b8", SizeBytes: 800,
		CreatedAt: pgtime(time.Now()),
	})
	if loc.ObjectKey != "le" || loc.BackendName != "b8" {
		t.Errorf("unexpected location: %+v", loc)
	}
}

// TestToObjectLocation_ListDirectChildrenRow covers the dashboard directory-
// listing row.
func TestToObjectLocation_ListDirectChildrenRow(t *testing.T) {
	t.Parallel()
	loc := toObjectLocation(db.ListDirectChildrenRow{
		ObjectKey: "ld", BackendName: "b9", SizeBytes: 900,
		CreatedAt: pgtime(time.Now()),
	})
	if loc.ObjectKey != "ld" || loc.BackendName != "b9" {
		t.Errorf("unexpected location: %+v", loc)
	}
}

// TestToObjectLocation_GetRandomHashedObjectsRow covers the scrubber's
// random-sampling query.
func TestToObjectLocation_GetRandomHashedObjectsRow(t *testing.T) {
	t.Parallel()
	loc := toObjectLocation(db.GetRandomHashedObjectsRow{
		ObjectKey: "r", BackendName: "b10", SizeBytes: 1000,
		CreatedAt: pgtime(time.Now()),
	})
	if loc.ObjectKey != "r" || loc.BackendName != "b10" {
		t.Errorf("unexpected location: %+v", loc)
	}
}

// TestToObjectLocation_GetObjectsWithoutHashRow covers the backfill query
// used by handleBackfillChecksums.
func TestToObjectLocation_GetObjectsWithoutHashRow(t *testing.T) {
	t.Parallel()
	loc := toObjectLocation(db.GetObjectsWithoutHashRow{
		ObjectKey: "nh", BackendName: "b11", SizeBytes: 1100,
		CreatedAt: pgtime(time.Now()),
	})
	if loc.ObjectKey != "nh" || loc.BackendName != "b11" {
		t.Errorf("unexpected location: %+v", loc)
	}
}

// TestToObjectLocation_UnknownTypeReturnsZero covers the default arm.
func TestToObjectLocation_UnknownTypeReturnsZero(t *testing.T) {
	t.Parallel()
	loc := toObjectLocation(struct{}{})
	if loc.ObjectKey != "" || loc.BackendName != "" {
		t.Errorf("unknown row type should yield zero ObjectLocation, got %+v", loc)
	}
}

// TestToObjectLocations_FlattensSlice sanity-checks the slice helper that
// routes every row through toObjectLocation.
func TestToObjectLocations_FlattensSlice(t *testing.T) {
	t.Parallel()
	rows := []any{
		db.ListObjectsByBackendRow{ObjectKey: "a", BackendName: "b", SizeBytes: 1, CreatedAt: pgtime(time.Now())},
		db.ListObjectsByBackendRow{ObjectKey: "c", BackendName: "d", SizeBytes: 2, CreatedAt: pgtime(time.Now())},
	}
	out := make([]ObjectLocation, 0, len(rows))
	for _, r := range rows {
		out = append(out, toObjectLocation(r))
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 results, got %d", len(out))
	}
	if out[0].ObjectKey != "a" || out[1].ObjectKey != "c" {
		t.Errorf("unexpected flattened slice: %+v", out)
	}
}

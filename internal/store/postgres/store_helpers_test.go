// -------------------------------------------------------------------------------
// store row -> ObjectLocation conversion tests
//
// Author: Alex Freidah
//
// Exercises the generic toFatObjectLocations and toSlimObjectLocations
// helpers without standing up a real PostgreSQL. Each row type the queries
// project has its own accessor methods (in internal/store/postgres/sqlc), and these
// tests assert the conversion for every shape  -  including the nullable
// pointer fields on the encryption-aware ("fat") rows.
// -------------------------------------------------------------------------------

package postgres

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// pgtime returns a pgx timestamptz wrapping t, valid and non-zero.
func pgtime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// -------------------------------------------------------------------------
// Fat-row conversions: encryption + content-hash columns
// -------------------------------------------------------------------------

// TestToFatObjectLocations_AllFieldsPopulated verifies every nullable
// pointer column is dereferenced when set.
func TestToFatObjectLocations_AllFieldsPopulated(t *testing.T) {
	t.Parallel()
	keyID := "key-abc"
	var ptSize int64 = 4096
	hash := "sha256:deadbeef"
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	out := toFatObjectLocations([]db.GetAllObjectLocationsRow{{
		ObjectKey:     "obj/foo",
		BackendName:   "b1",
		SizeBytes:     4200,
		Encrypted:     true,
		EncryptionKey: []byte{0xde, 0xad, 0xbe, 0xef},
		KeyID:         &keyID,
		PlaintextSize: &ptSize,
		ContentHash:   &hash,
		CreatedAt:     pgtime(created),
	}})
	if len(out) != 1 {
		t.Fatalf("expected 1 location, got %d", len(out))
	}
	loc := out[0]

	if loc.ObjectKey != "obj/foo" || loc.BackendName != "b1" || loc.SizeBytes != 4200 {
		t.Errorf("basic fields: %+v", loc)
	}
	if !loc.Encrypted || loc.KeyID != "key-abc" {
		t.Errorf("encryption fields: %+v", loc)
	}
	if loc.PlaintextSize != 4096 || loc.ContentHash != "sha256:deadbeef" {
		t.Errorf("nullable fields: %+v", loc)
	}
	if !loc.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", loc.CreatedAt, created)
	}
}

// TestToFatObjectLocations_NilNullableFields verifies the helper does not
// panic when KeyID, PlaintextSize, or ContentHash are nil.
func TestToFatObjectLocations_NilNullableFields(t *testing.T) {
	t.Parallel()
	out := toFatObjectLocations([]db.GetAllObjectLocationsRow{{
		ObjectKey:   "k",
		BackendName: "b",
		SizeBytes:   1,
		CreatedAt:   pgtime(time.Now()),
		// Encrypted=false, EncryptionKey nil, KeyID/PlaintextSize/ContentHash nil
	}})
	loc := out[0]
	if loc.KeyID != "" || loc.PlaintextSize != 0 || loc.ContentHash != "" {
		t.Errorf("nil pointer fields not zeroed: %+v", loc)
	}
	if loc.Encrypted {
		t.Errorf("Encrypted should be false")
	}
}

// TestToFatObjectLocations_PartialNilFields covers the mid-state where some
// nullable fields are set and others are not.
func TestToFatObjectLocations_PartialNilFields(t *testing.T) {
	t.Parallel()
	keyID := "k1"
	out := toFatObjectLocations([]db.GetAllObjectLocationsRow{{
		ObjectKey:     "k",
		BackendName:   "b",
		SizeBytes:     1,
		Encrypted:     true,
		EncryptionKey: []byte{0x01},
		KeyID:         &keyID,
		// PlaintextSize/ContentHash nil
		CreatedAt: pgtime(time.Now()),
	}})
	loc := out[0]
	if loc.KeyID != "k1" {
		t.Errorf("KeyID = %q, want k1", loc.KeyID)
	}
	if loc.PlaintextSize != 0 || loc.ContentHash != "" {
		t.Errorf("nil pointer fields not zeroed: %+v", loc)
	}
}

// TestToFatObjectLocations_EveryRowType walks each fat-row sqlc type to
// confirm the accessor methods are wired and the generic helper accepts
// each one. A zero-element entry per type is enough  -  the per-field
// flattening is exhaustively tested for GetAllObjectLocationsRow above.
func TestToFatObjectLocations_EveryRowType(t *testing.T) {
	t.Parallel()
	now := pgtime(time.Now())

	if got := toFatObjectLocations([]db.GetAllObjectLocationsRow{{ObjectKey: "a", CreatedAt: now}}); got[0].ObjectKey != "a" {
		t.Error("GetAllObjectLocationsRow not converted")
	}
	if got := toFatObjectLocations([]db.GetUnderReplicatedObjectsRow{{ObjectKey: "b", CreatedAt: now}}); got[0].ObjectKey != "b" {
		t.Error("GetUnderReplicatedObjectsRow not converted")
	}
	if got := toFatObjectLocations([]db.GetUnderReplicatedObjectsExcludingRow{{ObjectKey: "c", CreatedAt: now}}); got[0].ObjectKey != "c" {
		t.Error("GetUnderReplicatedObjectsExcludingRow not converted")
	}
	if got := toFatObjectLocations([]db.GetOverReplicatedObjectsRow{{ObjectKey: "d", CreatedAt: now}}); got[0].ObjectKey != "d" {
		t.Error("GetOverReplicatedObjectsRow not converted")
	}
	if got := toFatObjectLocations([]db.GetLeastRecentlyScrubbedObjectsRow{{ObjectKey: "f", CreatedAt: now}}); got[0].ObjectKey != "f" {
		t.Error("GetRandomHashedObjectsRow not converted")
	}
	if got := toFatObjectLocations([]db.GetObjectsWithoutHashRow{{ObjectKey: "g", CreatedAt: now}}); got[0].ObjectKey != "g" {
		t.Error("GetObjectsWithoutHashRow not converted")
	}
}

// -------------------------------------------------------------------------
// Slim-row conversions: key/backend/size/created_at only
// -------------------------------------------------------------------------

// TestToSlimObjectLocations_EveryRowType walks each slim-row sqlc type.
// Slim rows have no encryption columns, so the resulting ObjectLocation
// leaves them at zero values.
func TestToSlimObjectLocations_EveryRowType(t *testing.T) {
	t.Parallel()
	now := pgtime(time.Now())

	if got := toSlimObjectLocations([]db.ListObjectsByBackendRow{{ObjectKey: "a", BackendName: "b1", SizeBytes: 1, CreatedAt: now}}); got[0].ObjectKey != "a" || got[0].Encrypted {
		t.Errorf("ListObjectsByBackendRow not converted: %+v", got[0])
	}
	if got := toSlimObjectLocations([]db.ListObjectsByPrefixRow{{ObjectKey: "b", BackendName: "b2", SizeBytes: 2, CreatedAt: now}}); got[0].ObjectKey != "b" {
		t.Error("ListObjectsByPrefixRow not converted")
	}
	if got := toSlimObjectLocations([]db.ListExpiredObjectsRow{{ObjectKey: "c", BackendName: "b3", SizeBytes: 3, CreatedAt: now}}); got[0].ObjectKey != "c" {
		t.Error("ListExpiredObjectsRow not converted")
	}
	// ListDirectChildrenRow groups by object_key with a backend_names array
	// and is intentionally not part of the slim-row projection set.
}

// TestToSlimObjectLocations_PreservesOrder verifies the output slice keeps
// the input ordering  -  important for the dashboard directory listing.
func TestToSlimObjectLocations_PreservesOrder(t *testing.T) {
	t.Parallel()
	now := pgtime(time.Now())
	out := toSlimObjectLocations([]db.ListObjectsByBackendRow{
		{ObjectKey: "a", BackendName: "b", SizeBytes: 1, CreatedAt: now},
		{ObjectKey: "b", BackendName: "b", SizeBytes: 2, CreatedAt: now},
		{ObjectKey: "c", BackendName: "b", SizeBytes: 3, CreatedAt: now},
	})
	if len(out) != 3 || out[0].ObjectKey != "a" || out[2].ObjectKey != "c" {
		t.Errorf("order not preserved: %+v", out)
	}
}

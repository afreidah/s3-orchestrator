// -------------------------------------------------------------------------------
// store row -> ObjectLocation conversion tests
//
// Author: Alex Freidah
//
// Exercises toObjectLocation and the generic toSlimObjectLocations without
// standing up a real PostgreSQL: every column of a whole row reaches the copy,
// a NULL column leaves its zero value, and each slim row type converts.
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
// WHOLE ROWS
// -------------------------------------------------------------------------

// TestToObjectLocation_AllFieldsPopulated verifies every column of a whole row
// reaches the copy, the nullable ones dereferenced.
func TestToObjectLocation_AllFieldsPopulated(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	scrubbed := created.Add(time.Hour)

	loc := toObjectLocation(&db.ObjectLocation{
		ObjectKey:             "obj/foo",
		BackendName:           "b1",
		StorageKey:            "sk",
		SizeBytes:             4200,
		Encrypted:             true,
		EncryptionKey:         []byte{0xde, 0xad},
		KeyID:                 new("key-abc"),
		PlaintextSize:         new(int64(4096)),
		ContentHash:           new("sha256:deadbeef"),
		CompressionProbeSize:  new(int64(900)),
		CompressionProbeLevel: new("default"),
		Etag:                  new(`"e1"`),
		ContentType:           new("text/plain"),
		UserMetadata:          []byte(`{"a":"b"}`),
		Managed:               false,
		CreatedAt:             pgtime(created),
		LastScrubbedAt:        pgtime(scrubbed),
	})

	if loc.ObjectKey != "obj/foo" || loc.BackendName != "b1" || loc.StorageKey != "sk" || loc.SizeBytes != 4200 {
		t.Errorf("basic fields: %+v", loc)
	}
	if !loc.Encrypted || loc.KeyID != "key-abc" || loc.PlaintextSize != 4096 || loc.ContentHash != "sha256:deadbeef" {
		t.Errorf("stored form: %+v", loc.StoredForm)
	}
	if loc.CompressionProbeSize != 900 || loc.CompressionProbeLevel != "default" {
		t.Errorf("probe = %d/%q, want 900/default", loc.CompressionProbeSize, loc.CompressionProbeLevel)
	}
	if loc.Identity == nil || loc.Identity.ETag != `"e1"` || loc.Identity.ContentType != "text/plain" || loc.Identity.UserMetadata["a"] != "b" {
		t.Errorf("identity = %+v", loc.Identity)
	}
	if !loc.Unmanaged {
		t.Error("an unmanaged row converted as managed")
	}
	if !loc.CreatedAt.Equal(created) || loc.LastScrubbedAt == nil || !loc.LastScrubbedAt.Equal(scrubbed) {
		t.Errorf("times = %v/%v, want %v/%v", loc.CreatedAt, loc.LastScrubbedAt, created, scrubbed)
	}
}

// TestToObjectLocation_NullColumns verifies a row whose nullable columns are
// NULL converts to their zero values, with no identity and no scrub time.
func TestToObjectLocation_NullColumns(t *testing.T) {
	t.Parallel()
	loc := toObjectLocation(&db.ObjectLocation{ObjectKey: "k", BackendName: "b", Managed: true, CreatedAt: pgtime(time.Now())})
	if loc.KeyID != "" || loc.PlaintextSize != 0 || loc.ContentHash != "" || loc.Encrypted {
		t.Errorf("NULL columns not zeroed: %+v", loc.StoredForm)
	}
	if loc.Identity != nil || loc.LastScrubbedAt != nil || loc.Unmanaged {
		t.Errorf("identity %v, scrubbed %v, unmanaged %v; want nil, nil, false", loc.Identity, loc.LastScrubbedAt, loc.Unmanaged)
	}
}

// -------------------------------------------------------------------------
// SLIM ROWS
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

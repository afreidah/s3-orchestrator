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
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestBackendFilter_UnencryptedLocations asserts the encrypt-existing listing
// selects one backend's copies when named and every backend when not.
func TestBackendFilter_UnencryptedLocations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/b", "backend-b", 200)

	all, err := s.ListUnencryptedLocations(ctx, 10, core.Cursor{}, "")
	if err != nil {
		t.Fatalf("ListUnencryptedLocations: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("fleet-wide = %d rows, want 2", len(all))
	}

	scoped, err := s.ListUnencryptedLocations(ctx, 10, core.Cursor{}, "backend-a")
	if err != nil {
		t.Fatalf("ListUnencryptedLocations(backend-a): %v", err)
	}
	if len(scoped) != 1 || scoped[0].BackendName != "backend-a" {
		t.Errorf("scoped = %+v, want one row on backend-a", scoped)
	}
}

// TestBackendFilter_ObjectsWithoutHash asserts the backfill listing scopes the
// same way.
func TestBackendFilter_ObjectsWithoutHash(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/b", "backend-b", 200)

	all, err := s.GetObjectsWithoutHash(ctx, 10, 0, "")
	if err != nil {
		t.Fatalf("GetObjectsWithoutHash: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("fleet-wide = %d rows, want 2", len(all))
	}

	scoped, err := s.GetObjectsWithoutHash(ctx, 10, 0, "backend-b")
	if err != nil {
		t.Fatalf("GetObjectsWithoutHash(backend-b): %v", err)
	}
	if len(scoped) != 1 || scoped[0].BackendName != "backend-b" {
		t.Errorf("scoped = %+v, want one row on backend-b", scoped)
	}
}

// TestBackendFilter_UnknownBackendSelectsNothing asserts a name no copy carries
// returns an empty page rather than every row, which is what would happen if
// the filter were dropped from the predicate.
func TestBackendFilter_UnknownBackendSelectsNothing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)

	rows, err := s.ListUnencryptedLocations(ctx, 10, core.Cursor{}, "backend-absent")
	if err != nil {
		t.Fatalf("ListUnencryptedLocations: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}
}

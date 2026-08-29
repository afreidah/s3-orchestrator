// -------------------------------------------------------------------------------
// SQLite Store Tests - Full Role Interface Contract Coverage
//
// Author: Alex Freidah
//
// Comprehensive tests for the SQLite store backend using in-memory databases.
// Covers object CRUD, quota enforcement, multipart uploads, replication,
// cleanup queue, usage tracking, directory listing, integrity verification,
// encryption admin, notification outbox, and advisory lock emulation.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TEST HELPERS
// -------------------------------------------------------------------------

// newTestStore creates an in-memory SQLite store for testing.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := NewStore(ctx, &config.DatabaseConfig{
		Driver: "sqlite",
		Path:   ":memory:",
	}, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Seed a backend quota entry for tests that need one.
	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{
		{Name: "backend-a", QuotaBytes: 1 << 30},
		{Name: "backend-b", QuotaBytes: 1 << 30},
	}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	return s
}

// rewindToSchemaVersion returns a test database to the shape it had at an
// earlier schema version, so the migration runner can be exercised against a
// database that genuinely predates a migration.
//
// Stamping the version alone is not enough. newTestStore builds the database
// from schema.sql, which carries every column the current version defines, so
// a rewound-but-unaltered database is a state no deployment ever reaches: an
// old version number over a current schema. Re-running an additive migration
// against it fails on a column that is already there.
//
// Each additive migration therefore needs its columns undone here to be
// rewound past.
func rewindToSchemaVersion(t *testing.T, s *Store, version int) {
	t.Helper()
	ctx := context.Background()

	if version < 10 {
		dropColumns(t, s, "multipart_uploads", "tagging")
	}

	// A table rather than a column, so the rewind drops the whole thing: the
	// migration under test creates it, and CREATE TABLE IF NOT EXISTS would
	// otherwise be a no-op against the baseline schema.
	if version < 9 {
		if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS object_tags`); err != nil {
			t.Fatalf("rewind: drop object_tags: %v", err)
		}
	}

	if version < 8 {
		dropColumns(t, s, "object_locations", "compression_probe_size", "compression_probe_level")
	}

	if version < 7 {
		for _, table := range []string{"object_locations", "pending_objects"} {
			dropColumns(t, s, table, "compression_algorithm", "compression_level",
				"compression_format_version", "logical_size")
		}
	}

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM schema_version; INSERT INTO schema_version (version) VALUES (?)`,
		version); err != nil {
		t.Fatalf("rewind schema version to %d: %v", version, err)
	}
}

// dropColumns removes columns schema.sql creates but an older database predates,
// so a rewound store is one the migration under test has real work to do on.
func dropColumns(t *testing.T, s *Store, table string, columns ...string) {
	t.Helper()
	for _, column := range columns {
		if _, err := s.db.ExecContext(context.Background(),
			`ALTER TABLE `+table+` DROP COLUMN `+column); err != nil {
			t.Fatalf("drop %s.%s: %v", table, column, err)
		}
	}
}

// mustRecordObject records an object, failing the test on error.
func mustRecordObject(t *testing.T, s *Store, key, backend string, size int64) {
	t.Helper()
	if _, err := s.RecordObject(context.Background(), &core.RecordObjectRequest{Key: key, Backend: backend, Size: size}); err != nil {
		t.Fatalf("RecordObject(%s, %s): %v", key, backend, err)
	}
}

// mustCreateUpload creates a multipart upload, failing the test on error.
func mustCreateUpload(t *testing.T, s *Store, uploadID, key, backend string) {
	t.Helper()
	if err := s.CreateMultipartUpload(context.Background(), &core.CreateMultipartUploadParams{
		UploadID:    uploadID,
		ObjectKey:   key,
		BackendName: backend,
	}); err != nil {
		t.Fatalf("CreateMultipartUpload(%s): %v", uploadID, err)
	}
}

// mustRecordReplica records a replica, failing the test on error. The
// size parameter is unused after #652  -  the SQL now reads size from the
// source row inside the conditional INSERT  -  but the helper signature
// keeps it so call sites continue to document expected size at a glance.
func mustRecordReplica(t *testing.T, s *Store, key, target, source string, _ int64) {
	t.Helper()
	if _, _, err := s.RecordReplica(context.Background(), key, target, source); err != nil {
		t.Fatalf("RecordReplica(%s, %s): %v", key, target, err)
	}
}

// mustEnqueueCleanup enqueues a cleanup item, failing the test on error.
func mustEnqueueCleanup(t *testing.T, s *Store, backend, key string) {
	t.Helper()
	if err := s.EnqueueCleanup(context.Background(), backend, key, "test", 256); err != nil {
		t.Fatalf("EnqueueCleanup(%s): %v", key, err)
	}
}

// mustInsertNotification inserts a notification, failing the test on error.
func mustInsertNotification(t *testing.T, s *Store, eventType, payload, url string) {
	t.Helper()
	if err := s.InsertNotification(context.Background(), eventType, payload, url); err != nil {
		t.Fatalf("InsertNotification: %v", err)
	}
}

// -------------------------------------------------------------------------
// OBJECT OPERATIONS
// -------------------------------------------------------------------------

// TestRecordObject_And_GetAllLocations verifies the record object and get all locations contract.
// Asserts that RecordObject:.
func TestRecordObject_And_GetAllLocations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	displaced, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/key1", Backend: "backend-a", Size: 1024})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if len(displaced) != 0 {
		t.Errorf("expected no displaced copies, got %d", len(displaced))
	}

	locs, err := s.GetAllObjectLocations(ctx, "bucket/key1")
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("expected 1 location, got %d", len(locs))
	}
	if locs[0].BackendName != "backend-a" || locs[0].SizeBytes != 1024 {
		t.Errorf("unexpected location: %+v", locs[0])
	}
}

// TestRecordObject_Overwrite_DisplacesCopy verifies that re-recording an object on a different backend returns the displaced copy.
func TestRecordObject_Overwrite_DisplacesCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/key1", "backend-a", 1024)

	// Overwrite on a different backend
	displaced, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/key1", Backend: "backend-b", Size: 2048})
	if err != nil {
		t.Fatalf("RecordObject overwrite: %v", err)
	}
	if len(displaced) != 1 {
		t.Fatalf("expected 1 displaced copy, got %d", len(displaced))
	}
	if displaced[0].BackendName != "backend-a" {
		t.Errorf("displaced backend = %q, want backend-a", displaced[0].BackendName)
	}
}

// TestDeleteObject verifies the delete object contract.
// Asserts that DeleteObject:.
func TestDeleteObject(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/key1", "backend-a", 1024)

	deleted, err := s.DeleteObject(ctx, "bucket/key1")
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if len(deleted) != 1 {
		t.Errorf("expected 1 deleted copy, got %d", len(deleted))
	}

	_, err = s.GetAllObjectLocations(ctx, "bucket/key1")
	if err != core.ErrObjectNotFound {
		t.Errorf("expected ErrObjectNotFound, got %v", err)
	}
}

// TestDeleteObject_NotFound verifies that deleting a nonexistent object returns ErrObjectNotFound.
func TestDeleteObject_NotFound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.DeleteObject(ctx, "bucket/nonexistent")
	if err != core.ErrObjectNotFound {
		t.Errorf("expected ErrObjectNotFound, got %v", err)
	}
}

// TestListObjects verifies prefix-scoped listing returns only matching objects.
func TestListObjects(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/b", "backend-a", 200)
	mustRecordObject(t, s, "bucket/c", "backend-a", 300)
	mustRecordObject(t, s, "other/x", "backend-a", 400)

	result, err := s.ListObjects(ctx, "bucket/", "", 10)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(result.Objects) != 3 {
		t.Errorf("expected 3 objects, got %d", len(result.Objects))
	}
	if result.IsTruncated {
		t.Error("should not be truncated")
	}
}

// TestListObjects_Pagination verifies continuation-token based pagination.
func TestListObjects_Pagination(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/b", "backend-a", 200)
	mustRecordObject(t, s, "bucket/c", "backend-a", 300)

	result, err := s.ListObjects(ctx, "bucket/", "", 2)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(result.Objects) != 2 {
		t.Errorf("expected 2 objects, got %d", len(result.Objects))
	}
	if !result.IsTruncated {
		t.Error("should be truncated")
	}

	// Second page
	result2, err := s.ListObjects(ctx, "bucket/", result.NextContinuationToken, 2)
	if err != nil {
		t.Fatalf("ListObjects page 2: %v", err)
	}
	if len(result2.Objects) != 1 {
		t.Errorf("expected 1 object on page 2, got %d", len(result2.Objects))
	}
}

// TestListObjectsByBackend verifies the list objects by backend contract.
// Asserts that ListObjectsByBackend:.
func TestListObjectsByBackend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/b", "backend-b", 200)

	locs, err := s.ListObjectsByBackend(ctx, "backend-a", 10)
	if err != nil {
		t.Fatalf("ListObjectsByBackend: %v", err)
	}
	if len(locs) != 1 {
		t.Errorf("expected 1, got %d", len(locs))
	}
}

// TestListObjectsByBackendKeyAsc_FirstPage drives the cursor's empty-string
// initial call: returns the lex-smallest rows for the backend in order.
func TestListObjectsByBackendKeyAsc_FirstPage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "vb/c", "backend-a", 30)
	mustRecordObject(t, s, "vb/a", "backend-a", 10)
	mustRecordObject(t, s, "vb/b", "backend-a", 20)
	mustRecordObject(t, s, "vb/x", "backend-b", 99) // different backend

	got, err := s.ListObjectsByBackendKeyAsc(ctx, "backend-a", "", 10)
	if err != nil {
		t.Fatalf("ListObjectsByBackendKeyAsc: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (be1 only)", len(got))
	}
	want := []string{"vb/a", "vb/b", "vb/c"}
	for i, w := range want {
		if got[i].ObjectKey != w {
			t.Errorf("got[%d] = %q, want %q (lex order)", i, got[i].ObjectKey, w)
		}
	}
}

// TestListObjectsByBackendKeyAsc_HonoursCursor verifies the > $afterKey
// filter  -  successive pages skip rows already returned.
func TestListObjectsByBackendKeyAsc_HonoursCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	for _, k := range []string{"vb/a", "vb/b", "vb/c", "vb/d"} {
		mustRecordObject(t, s, k, "backend-a", 1)
	}

	// First page: cursor "" -> ["vb/a", "vb/b"]
	page1, err := s.ListObjectsByBackendKeyAsc(ctx, "backend-a", "", 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 || page1[0].ObjectKey != "vb/a" || page1[1].ObjectKey != "vb/b" {
		t.Fatalf("page1 unexpected: %+v", page1)
	}

	// Second page: cursor "vb/b" -> ["vb/c", "vb/d"]
	page2, err := s.ListObjectsByBackendKeyAsc(ctx, "backend-a", page1[1].ObjectKey, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 || page2[0].ObjectKey != "vb/c" || page2[1].ObjectKey != "vb/d" {
		t.Fatalf("page2 unexpected: %+v", page2)
	}

	// Third page: cursor "vb/d" -> empty
	page3, err := s.ListObjectsByBackendKeyAsc(ctx, "backend-a", page2[1].ObjectKey, 2)
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(page3) != 0 {
		t.Errorf("page3 should be empty, got %v", page3)
	}
}

// TestListObjectsByBackendKeyAsc_RespectsLimit confirms the LIMIT clause
// is honoured even when more rows match.
func TestListObjectsByBackendKeyAsc_RespectsLimit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	for _, k := range []string{"vb/a", "vb/b", "vb/c"} {
		mustRecordObject(t, s, k, "backend-a", 1)
	}
	got, err := s.ListObjectsByBackendKeyAsc(ctx, "backend-a", "", 1)
	if err != nil {
		t.Fatalf("ListObjectsByBackendKeyAsc: %v", err)
	}
	if len(got) != 1 || got[0].ObjectKey != "vb/a" {
		t.Errorf("limit not honoured: %+v", got)
	}
}

// TestImportObject verifies that importing a pre-existing object records it correctly.
func TestImportObject(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	imported, err := s.ImportObject(ctx, "bucket/new", "backend-a", 500, false, nil)
	if err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	if !imported {
		t.Error("expected imported=true for new object")
	}

	// Import again should be a no-op
	imported, err = s.ImportObject(ctx, "bucket/new", "backend-a", 500, false, nil)
	if err != nil {
		t.Fatalf("ImportObject duplicate: %v", err)
	}
	if imported {
		t.Error("expected imported=false for duplicate")
	}
}

// TestMoveObjectLocation verifies the move object location contract.
// Asserts that MoveObjectLocation:.
func TestMoveObjectLocation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/key1", "backend-a", 1024)

	moved, err := s.MoveObjectLocation(ctx, "bucket/key1", "backend-a", "backend-b")
	if err != nil {
		t.Fatalf("MoveObjectLocation: %v", err)
	}
	if moved != 1024 {
		t.Errorf("moved = %d, want 1024", moved)
	}

	locs, _ := s.GetAllObjectLocations(ctx, "bucket/key1")
	if len(locs) != 1 || locs[0].BackendName != "backend-b" {
		t.Errorf("expected object on backend-b, got %+v", locs)
	}
}

// TestMoveObjectLocation_TargetAlreadyHasCopy verifies the short-circuit
// when the destination already holds a copy  -  MoveObjectLocation returns
// (0, nil) without touching the source.
func TestMoveObjectLocation_TargetAlreadyHasCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/dup", "backend-a", 100)
	mustRecordReplica(t, s, "bucket/dup", "backend-b", "backend-a", 100)

	moved, err := s.MoveObjectLocation(ctx, "bucket/dup", "backend-a", "backend-b")
	if err != nil {
		t.Fatalf("MoveObjectLocation: %v", err)
	}
	if moved != 0 {
		t.Errorf("moved = %d, want 0 (target already has copy)", moved)
	}
}

// TestMoveObjectLocation_SourceGone verifies the benign no-op when the
// source row has already been removed  -  MoveObjectLocation returns
// (0, nil) rather than an error.
func TestMoveObjectLocation_SourceGone(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	moved, err := s.MoveObjectLocation(ctx, "bucket/missing", "backend-a", "backend-b")
	if err != nil {
		t.Fatalf("MoveObjectLocation: %v", err)
	}
	if moved != 0 {
		t.Errorf("moved = %d, want 0 (no source copy)", moved)
	}
}

// TestRecordObject_Overwrite_SameBackend covers the branch in
// clearDisplacedCopies where the prior copy lives on the new target
// backend  -  no DeletedCopy should be returned because the PutObject will
// overwrite in place.
func TestRecordObject_Overwrite_SameBackend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/k", "backend-a", 500)

	displaced, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/k", Backend: "backend-a", Size: 700})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if len(displaced) != 0 {
		t.Errorf("expected 0 displaced (same backend), got %d: %+v", len(displaced), displaced)
	}
}

// TestMoveObjectLocation_QuotaExceeded covers the ErrNoSpaceAvailable
// branch in moveObjectRows  -  the destination quota update touches zero
// rows when the move would exceed bytes_limit.
func TestMoveObjectLocation_QuotaExceeded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := NewStore(ctx, &config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"}, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{
		{Name: "big", QuotaBytes: 10_000},
		{Name: "small", QuotaBytes: 100}, // tiny  -  cannot hold 500-byte object
	}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/huge", Backend: "big", Size: 500}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	_, err = s.MoveObjectLocation(ctx, "bucket/huge", "big", "small")
	if err != core.ErrNoSpaceAvailable {
		t.Errorf("expected ErrNoSpaceAvailable, got %v", err)
	}
}

// TestRecordObject_QuotaExceeded covers the ErrNoSpaceAvailable branch
// in incrementSQLiteQuota  -  the guarded UPDATE touches zero rows when
// the quota ceiling would be exceeded.
func TestRecordObject_QuotaExceeded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := NewStore(ctx, &config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"}, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{
		{Name: "tight", QuotaBytes: 100},
	}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	_, err = s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/over", Backend: "tight", Size: 500})
	if err != core.ErrNoSpaceAvailable {
		t.Errorf("expected ErrNoSpaceAvailable, got %v", err)
	}
}

// TestListDirectoryChildren_Pagination covers the hasMore branch: when
// more files exist under a prefix than maxKeys allows, the caller must
// see HasMore=true and a NextCursor pointing at the last returned entry.
func TestListDirectoryChildren_Pagination(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "b/a.txt", "backend-a", 10)
	mustRecordObject(t, s, "b/b.txt", "backend-a", 20)
	mustRecordObject(t, s, "b/c.txt", "backend-a", 30)

	result, err := s.ListDirectoryChildren(ctx, "b/", "", 2)
	if err != nil {
		t.Fatalf("ListDirectoryChildren: %v", err)
	}
	if !result.HasMore {
		t.Error("expected HasMore=true")
	}
	if result.NextCursor == "" {
		t.Error("expected non-empty NextCursor")
	}
}

// TestBackendObjectStats verifies per-backend object count and byte totals.
func TestBackendObjectStats(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/b", "backend-a", 200)

	count, total, err := s.BackendObjectStats(ctx, "backend-a")
	if err != nil {
		t.Fatalf("BackendObjectStats: %v", err)
	}
	if count != 2 || total != 300 {
		t.Errorf("count=%d total=%d, want 2/300", count, total)
	}
}

// -------------------------------------------------------------------------
// ENCRYPTION METADATA
// -------------------------------------------------------------------------

// TestRecordObject_WithEncryption verifies the record object with encryption contract.
// Asserts that RecordObject with encryption:.
func TestRecordObject_WithEncryption(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	form := &core.StoredForm{
		Encrypted:     true,
		EncryptionKey: []byte("wrapped-dek"),
		KeyID:         "key-1",
		PlaintextSize: 1024,
		ContentHash:   "abc123",
	}
	_, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/encrypted", Backend: "backend-a", Size: 1100, Form: form})
	if err != nil {
		t.Fatalf("RecordObject with encryption: %v", err)
	}

	locs, _ := s.GetAllObjectLocations(ctx, "bucket/encrypted")
	if !locs[0].Encrypted {
		t.Error("expected Encrypted=true")
	}
	if locs[0].KeyID != "key-1" {
		t.Errorf("KeyID = %q, want key-1", locs[0].KeyID)
	}
	if locs[0].ContentHash != "abc123" {
		t.Errorf("ContentHash = %q, want abc123", locs[0].ContentHash)
	}
}

// -------------------------------------------------------------------------
// QUOTA OPERATIONS
// -------------------------------------------------------------------------

// TestGetBackendWithSpace verifies the get backend with space contract.
// Asserts that GetBackendWithSpace:.
func TestGetBackendWithSpace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	name, err := s.GetBackendWithSpace(ctx, 100, []string{"backend-a", "backend-b"})
	if err != nil {
		t.Fatalf("GetBackendWithSpace: %v", err)
	}
	if name != "backend-a" {
		t.Errorf("expected backend-a (first with space), got %q", name)
	}
}

// TestGetLeastUtilizedBackend verifies the get least utilized backend contract.
// Asserts that GetLeastUtilizedBackend:.
func TestGetLeastUtilizedBackend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	// Put some data on backend-a
	mustRecordObject(t, s, "bucket/big", "backend-a", 500<<20)

	name, err := s.GetLeastUtilizedBackend(ctx, 100, []string{"backend-a", "backend-b"})
	if err != nil {
		t.Fatalf("GetLeastUtilizedBackend: %v", err)
	}
	if name != "backend-b" {
		t.Errorf("expected backend-b (less utilized), got %q", name)
	}
}

// TestGetLeastUtilizedBackend_FiltersByEligibleList confirms the IN clause
// genuinely filters: backend-a has more free space than backend-b, but
// the eligible list contains only backend-b, so backend-b must win.
func TestGetLeastUtilizedBackend_FiltersByEligibleList(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/big", "backend-b", 500<<20)

	name, err := s.GetLeastUtilizedBackend(ctx, 100, []string{"backend-b"})
	if err != nil {
		t.Fatalf("GetLeastUtilizedBackend: %v", err)
	}
	if name != "backend-b" {
		t.Errorf("eligible=[backend-b] forced selection failed, got %q", name)
	}
}

// TestGetLeastUtilizedBackend_RejectsUnknownEligibleNames asserts the IN
// filter does not silently fall back to a registered-but-not-eligible
// backend when the caller's list contains only unknown names.
func TestGetLeastUtilizedBackend_RejectsUnknownEligibleNames(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetLeastUtilizedBackend(ctx, 100, []string{"never-registered"})
	if !errors.Is(err, core.ErrNoSpaceAvailable) {
		t.Errorf("err = %v, want ErrNoSpaceAvailable", err)
	}
}

// TestGetQuotaStats verifies per-backend quota statistics retrieval.
func TestGetQuotaStats(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	stats, err := s.GetQuotaStats(ctx)
	if err != nil {
		t.Fatalf("GetQuotaStats: %v", err)
	}
	if len(stats) != 2 {
		t.Errorf("expected 2 backends, got %d", len(stats))
	}
}

// TestOrphanBytes verifies the orphan bytes contract.
// Asserts that IncrementOrphanBytes:.
func TestOrphanBytes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.IncrementOrphanBytes(ctx, "backend-a", 500); err != nil {
		t.Fatalf("IncrementOrphanBytes: %v", err)
	}

	stats, _ := s.GetQuotaStats(ctx)
	if stats["backend-a"].OrphanBytes != 500 {
		t.Errorf("orphan_bytes = %d, want 500", stats["backend-a"].OrphanBytes)
	}

	if err := s.DecrementOrphanBytes(ctx, "backend-a", 300); err != nil {
		t.Fatalf("DecrementOrphanBytes: %v", err)
	}

	stats, _ = s.GetQuotaStats(ctx)
	if stats["backend-a"].OrphanBytes != 200 {
		t.Errorf("orphan_bytes = %d, want 200", stats["backend-a"].OrphanBytes)
	}
}

// -------------------------------------------------------------------------
// USAGE TRACKING
// -------------------------------------------------------------------------

// TestFlushUsageDeltas_And_GetUsage verifies usage delta accumulation and flush to DB.
func TestFlushUsageDeltas_And_GetUsage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	period := "2026-03"
	if err := s.FlushUsageDeltas(ctx, "backend-a", period, 10, 1024, 2048); err != nil {
		t.Fatalf("FlushUsageDeltas: %v", err)
	}
	// Flush again to test accumulation
	if err := s.FlushUsageDeltas(ctx, "backend-a", period, 5, 512, 256); err != nil {
		t.Fatalf("FlushUsageDeltas second: %v", err)
	}

	usage, err := s.GetUsageForPeriod(ctx, period)
	if err != nil {
		t.Fatalf("GetUsageForPeriod: %v", err)
	}
	stat := usage["backend-a"]
	if stat.APIRequests != 15 {
		t.Errorf("api_requests = %d, want 15", stat.APIRequests)
	}
	if stat.EgressBytes != 1536 {
		t.Errorf("egress_bytes = %d, want 1536", stat.EgressBytes)
	}
}

// -------------------------------------------------------------------------
// MULTIPART UPLOADS
// -------------------------------------------------------------------------

// TestMultipartUpload_Lifecycle verifies the full create/record-part/complete/delete lifecycle.
func TestMultipartUpload_Lifecycle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	meta := map[string]string{"Content-Type": "image/png"}
	err := s.CreateMultipartUpload(ctx, &core.CreateMultipartUploadParams{
		UploadID:    "upload-1",
		ObjectKey:   "bucket/photo.png",
		BackendName: "backend-a",
		ContentType: "image/png",
		Metadata:    meta,
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	// Record parts
	if err := s.RecordPart(ctx, "upload-1", 1, "etag1", 1024, nil); err != nil {
		t.Fatalf("RecordPart 1: %v", err)
	}
	if err := s.RecordPart(ctx, "upload-1", 2, "etag2", 2048, nil); err != nil {
		t.Fatalf("RecordPart 2: %v", err)
	}

	// Get parts
	parts, err := s.GetParts(ctx, "upload-1")
	if err != nil {
		t.Fatalf("GetParts: %v", err)
	}
	if len(parts) != 2 {
		t.Errorf("expected 2 parts, got %d", len(parts))
	}

	// Get upload
	mu, err := s.GetMultipartUpload(ctx, "upload-1")
	if err != nil {
		t.Fatalf("GetMultipartUpload: %v", err)
	}
	if mu.BackendName != "backend-a" {
		t.Errorf("backend = %q, want backend-a", mu.BackendName)
	}

	// Verify metadata round-trip
	if mu.Metadata["Content-Type"] != "image/png" {
		t.Errorf("metadata Content-Type = %q", mu.Metadata["Content-Type"])
	}

	// Delete
	if err := s.DeleteMultipartUpload(ctx, "upload-1"); err != nil {
		t.Fatalf("DeleteMultipartUpload: %v", err)
	}

	_, err = s.GetMultipartUpload(ctx, "upload-1")
	if err != core.ErrMultipartUploadNotFound {
		t.Errorf("expected ErrMultipartUploadNotFound, got %v", err)
	}
}

// TestListMultipartUploads verifies prefix-scoped listing of active multipart uploads.
func TestListMultipartUploads(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateUpload(t, s, "u1", "bucket/a", "backend-a")
	mustCreateUpload(t, s, "u2", "bucket/b", "backend-a")
	mustCreateUpload(t, s, "u3", "other/c", "backend-a")

	uploads, err := s.ListMultipartUploads(ctx, "bucket/", 10)
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	if len(uploads) != 2 {
		t.Errorf("expected 2 uploads with prefix bucket/, got %d", len(uploads))
	}
}

// TestCountActiveMultipartUploads verifies the count active multipart uploads contract.
// Asserts that CountActiveMultipartUploads:.
func TestCountActiveMultipartUploads(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateUpload(t, s, "u1", "bucket/a", "backend-a")
	mustCreateUpload(t, s, "u2", "bucket/b", "backend-a")

	count, err := s.CountActiveMultipartUploads(ctx, "bucket/")
	if err != nil {
		t.Fatalf("CountActiveMultipartUploads: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

// TestGetActiveMultipartCounts verifies per-backend active multipart upload counts.
func TestGetActiveMultipartCounts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateUpload(t, s, "u1", "bucket/a", "backend-a")
	mustCreateUpload(t, s, "u2", "bucket/b", "backend-b")

	counts, err := s.GetActiveMultipartCounts(ctx)
	if err != nil {
		t.Fatalf("GetActiveMultipartCounts: %v", err)
	}
	if counts["backend-a"] != 1 || counts["backend-b"] != 1 {
		t.Errorf("unexpected counts: %v", counts)
	}
}

// -------------------------------------------------------------------------
// REPLICATION
// -------------------------------------------------------------------------

// TestReplication_UnderAndOver verifies detection of under- and over-replicated objects.
func TestReplication_UnderAndOver(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	// Record object on one backend
	mustRecordObject(t, s, "bucket/key1", "backend-a", 1024)

	// Should be under-replicated at factor 2
	under, err := s.GetUnderReplicatedObjects(ctx, 2, 10)
	if err != nil {
		t.Fatalf("GetUnderReplicatedObjects: %v", err)
	}
	if len(under) != 1 {
		t.Errorf("expected 1 under-replicated, got %d", len(under))
	}

	// Record replica
	size, inserted, err := s.RecordReplica(ctx, "bucket/key1", "backend-b", "backend-a")
	if err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}
	if !inserted {
		t.Error("expected replica to be inserted")
	}
	if size != 1024 {
		t.Errorf("expected recorded size 1024, got %d", size)
	}

	// Should no longer be under-replicated
	under, _ = s.GetUnderReplicatedObjects(ctx, 2, 10)
	if len(under) != 0 {
		t.Errorf("expected 0 under-replicated after replica, got %d", len(under))
	}

	// Should be over-replicated at factor 1
	over, err := s.GetOverReplicatedObjects(ctx, 1, 10)
	if err != nil {
		t.Fatalf("GetOverReplicatedObjects: %v", err)
	}
	if len(over) != 2 {
		t.Errorf("expected 2 copies (over at factor 1), got %d", len(over))
	}
}

// TestRecordReplica_Duplicate verifies the record replica duplicate contract.
// Asserts that RecordReplica duplicate:.
func TestRecordReplica_Duplicate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/key1", "backend-a", 1024)
	mustRecordReplica(t, s, "bucket/key1", "backend-b", "backend-a", 1024)

	// Duplicate replica should return false
	size, inserted, err := s.RecordReplica(ctx, "bucket/key1", "backend-b", "backend-a")
	if err != nil {
		t.Fatalf("RecordReplica duplicate: %v", err)
	}
	if inserted {
		t.Error("expected inserted=false for duplicate replica")
	}
	if size != 0 {
		t.Errorf("expected size 0 on duplicate, got %d", size)
	}
}

// TestRemoveExcessCopy verifies the remove excess copy contract.
// Asserts that RemoveExcessCopy:.
func TestRemoveExcessCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/key1", "backend-a", 1024)
	mustRecordReplica(t, s, "bucket/key1", "backend-b", "backend-a", 1024)

	removed, err := s.RemoveExcessCopy(ctx, "bucket/key1", "backend-b", 1)
	if err != nil {
		t.Fatalf("RemoveExcessCopy: %v", err)
	}
	if !removed {
		t.Fatalf("expected removed=true with 2 copies and factor=1")
	}

	locs, _ := s.GetAllObjectLocations(ctx, "bucket/key1")
	if len(locs) != 1 {
		t.Errorf("expected 1 copy after removal, got %d", len(locs))
	}
}

// TestRemoveExcessCopy_NoOpWhenAtFactor pins the race where another
// path (parallel client delete, factor raised mid-tick, an earlier
// cleaner tick on the same batch) has already brought the copy count
// down to factor before this tx acquires the lock. The re-read inside
// the tx sees count == factor and bails without deleting -- otherwise
// we under-replicate.
func TestRemoveExcessCopy_NoOpWhenAtFactor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/k-atfactor", "backend-a", 1024)

	removed, err := s.RemoveExcessCopy(ctx, "bucket/k-atfactor", "backend-a", 1)
	if err != nil {
		t.Fatalf("RemoveExcessCopy: %v", err)
	}
	if removed {
		t.Fatalf("expected removed=false when count==factor; would under-replicate")
	}

	locs, _ := s.GetAllObjectLocations(ctx, "bucket/k-atfactor")
	if len(locs) != 1 {
		t.Fatalf("expected 1 copy preserved, got %d", len(locs))
	}
}

// TestRemoveExcessCopy_NoOpWhenVictimGone pins the race where the
// scheduled victim copy has already been deleted by another path
// between the cleaner's scan and the per-victim tx. The locked re-read
// shows the victim missing; we no-op rather than blindly decrementing
// the quota for a row that no longer exists.
func TestRemoveExcessCopy_NoOpWhenVictimGone(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/k-vgone", "backend-a", 1024)
	mustRecordReplica(t, s, "bucket/k-vgone", "backend-b", "backend-a", 1024)

	// Simulate a parallel client landing the delete on backend-b
	// before the cleaner's per-victim tx executes.
	if err := s.DeleteObjectLocation(ctx, "bucket/k-vgone", "backend-b"); err != nil {
		t.Fatalf("setup DeleteObjectLocation: %v", err)
	}

	removed, err := s.RemoveExcessCopy(ctx, "bucket/k-vgone", "backend-b", 1)
	if err != nil {
		t.Fatalf("RemoveExcessCopy: %v", err)
	}
	if removed {
		t.Fatalf("expected removed=false when victim already gone")
	}

	locs, _ := s.GetAllObjectLocations(ctx, "bucket/k-vgone")
	if len(locs) != 1 {
		t.Fatalf("expected 1 copy preserved on backend-a, got %d", len(locs))
	}
	if locs[0].BackendName != "backend-a" {
		t.Errorf("expected surviving copy on backend-a, got %s", locs[0].BackendName)
	}
}

// TestReconcileUsage_CorrectsDrift records objects so the ledger total is
// known, corrupts the bytes_used counter, and verifies ReconcileUsage restores
// it to SUM(object_locations.size_bytes) and reports the applied delta.
func TestReconcileUsage_CorrectsDrift(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/k1", "backend-a", 100)
	mustRecordObject(t, s, "bucket/k2", "backend-a", 250)

	if _, err := s.rawDB.ExecContext(ctx,
		`UPDATE backend_quotas SET bytes_used = 99999 WHERE backend_name = 'backend-a'`); err != nil {
		t.Fatalf("corrupt bytes_used: %v", err)
	}

	adj, err := s.ReconcileUsage(ctx)
	if err != nil {
		t.Fatalf("ReconcileUsage: %v", err)
	}

	var got int64
	if err := s.rawDB.QueryRowContext(ctx,
		`SELECT bytes_used FROM backend_quotas WHERE backend_name = 'backend-a'`).Scan(&got); err != nil {
		t.Fatalf("read bytes_used: %v", err)
	}
	if got != 350 {
		t.Errorf("bytes_used = %d, want 350 (ledger truth)", got)
	}
	if adj["backend-a"] != 350-99999 {
		t.Errorf("adjustment = %d, want %d", adj["backend-a"], 350-99999)
	}
}

// -------------------------------------------------------------------------
// CLEANUP QUEUE
// -------------------------------------------------------------------------

// TestCleanupQueue_Lifecycle verifies enqueue, dequeue, and completion of cleanup items.
func TestCleanupQueue_Lifecycle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueCleanup(ctx, "backend-a", "bucket/orphan", "test", 512); err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}

	depth, _ := s.CleanupQueueDepth(ctx)
	if depth != 1 {
		t.Errorf("depth = %d, want 1", depth)
	}

	items, err := s.GetPendingCleanups(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 pending item, got %d", len(items))
	}
	if items[0].ObjectKey != "bucket/orphan" {
		t.Errorf("key = %q", items[0].ObjectKey)
	}

	// Complete it
	if err := s.CompleteCleanupItem(ctx, items[0].ID); err != nil {
		t.Fatalf("CompleteCleanupItem: %v", err)
	}

	depth, _ = s.CleanupQueueDepth(ctx)
	if depth != 0 {
		t.Errorf("depth after complete = %d, want 0", depth)
	}
}

// TestCleanupQueue_Retry verifies the cleanup queue retry contract.
// Asserts that RetryCleanupItem:.
func TestCleanupQueue_Retry(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustEnqueueCleanup(t, s, "backend-a", "bucket/retry")

	items, _ := s.GetPendingCleanups(ctx, 10)
	if err := s.RetryCleanupItem(ctx, items[0].ID, time.Hour, "connection refused"); err != nil {
		t.Fatalf("RetryCleanupItem: %v", err)
	}

	// Should not be pending (next_retry is in the future)
	items, _ = s.GetPendingCleanups(ctx, 10)
	if len(items) != 0 {
		t.Errorf("expected 0 pending after retry with future backoff, got %d", len(items))
	}
}

// TestSweepStaleCleanupQueueRows_RemovesMatchAndDecrementsOrphan verifies
// the sweep deletes every cleanup_queue row matching the (key, backend)
// pair and credits the bytes back to the backend's orphan_bytes counter.
func TestSweepStaleCleanupQueueRows_RemovesMatchAndDecrementsOrphan(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	// Two queue rows for the same key+backend, plus a row that should be
	// left untouched. EnqueueCleanup itself does not bump orphan_bytes
	// (the production code does that at the call sites that enqueue), so
	// we set it directly to simulate the steady state the sweep should
	// undo.
	if err := s.EnqueueCleanup(ctx, "backend-a", "bucket/k", "test", 100); err != nil {
		t.Fatalf("EnqueueCleanup #1: %v", err)
	}
	if err := s.EnqueueCleanup(ctx, "backend-a", "bucket/k", "retry", 200); err != nil {
		t.Fatalf("EnqueueCleanup #2: %v", err)
	}
	if err := s.EnqueueCleanup(ctx, "backend-a", "bucket/other", "test", 50); err != nil {
		t.Fatalf("EnqueueCleanup #3: %v", err)
	}
	if err := s.IncrementOrphanBytes(ctx, "backend-a", 350); err != nil {
		t.Fatalf("IncrementOrphanBytes: %v", err)
	}

	rows, err := s.SweepStaleCleanupQueueRows(ctx, "bucket/k", "backend-a")
	if err != nil {
		t.Fatalf("SweepStaleCleanupQueueRows: %v", err)
	}
	if rows != 2 {
		t.Errorf("rows deleted = %d, want 2", rows)
	}

	// Untouched row remains.
	depth, _ := s.CleanupQueueDepth(ctx)
	if depth != 1 {
		t.Errorf("queue depth = %d, want 1 (only bucket/other left)", depth)
	}

	// orphan_bytes for backend-a: 350 - (100+200) = 50.
	stats, err := s.GetQuotaStats(ctx)
	if err != nil {
		t.Fatalf("GetQuotaStats: %v", err)
	}
	if got := stats["backend-a"].OrphanBytes; got != 50 {
		t.Errorf("orphan_bytes = %d, want 50", got)
	}
}

// TestSweepStaleCleanupQueueRows_NoMatchIsNoOp verifies that calling the
// sweep when no rows match returns 0 and does not touch orphan_bytes.
func TestSweepStaleCleanupQueueRows_NoMatchIsNoOp(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.IncrementOrphanBytes(ctx, "backend-a", 100); err != nil {
		t.Fatalf("IncrementOrphanBytes: %v", err)
	}

	rows, err := s.SweepStaleCleanupQueueRows(ctx, "bucket/missing", "backend-a")
	if err != nil {
		t.Fatalf("SweepStaleCleanupQueueRows: %v", err)
	}
	if rows != 0 {
		t.Errorf("rows = %d, want 0 for no-match sweep", rows)
	}
	stats, _ := s.GetQuotaStats(ctx)
	if got := stats["backend-a"].OrphanBytes; got != 100 {
		t.Errorf("orphan_bytes = %d, want 100 (untouched)", got)
	}
}

// TestSweepStaleCleanupQueueRows_OnlyOtherBackend verifies the sweep
// only touches the requested backend's rows; same-key rows on other
// backends are left alone.
func TestSweepStaleCleanupQueueRows_OnlyOtherBackend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueCleanup(ctx, "backend-a", "bucket/k", "test", 100); err != nil {
		t.Fatalf("EnqueueCleanup a: %v", err)
	}
	if err := s.EnqueueCleanup(ctx, "backend-b", "bucket/k", "test", 200); err != nil {
		t.Fatalf("EnqueueCleanup b: %v", err)
	}

	rows, err := s.SweepStaleCleanupQueueRows(ctx, "bucket/k", "backend-a")
	if err != nil {
		t.Fatalf("SweepStaleCleanupQueueRows: %v", err)
	}
	if rows != 1 {
		t.Errorf("rows = %d, want 1 (only backend-a row)", rows)
	}
	depth, _ := s.CleanupQueueDepth(ctx)
	if depth != 1 {
		t.Errorf("queue depth = %d, want 1 (backend-b row preserved)", depth)
	}
}

// -------------------------------------------------------------------------
// INTEGRITY
// -------------------------------------------------------------------------

// TestIntegrity_HashOperations verifies the integrity hash operations contract.
// Asserts that GetObjectsWithoutHash:.
func TestIntegrity_HashOperations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/b", "backend-a", 200)

	// Both should be without hash
	unhashed, err := s.GetObjectsWithoutHash(ctx, 10, 0)
	if err != nil {
		t.Fatalf("GetObjectsWithoutHash: %v", err)
	}
	if len(unhashed) != 2 {
		t.Errorf("expected 2 unhashed, got %d", len(unhashed))
	}

	// Update hash
	if err := s.UpdateContentHash(ctx, "bucket/a", "backend-a", "sha256:abc"); err != nil {
		t.Fatalf("UpdateContentHash: %v", err)
	}

	// Now only 1 without hash
	unhashed, _ = s.GetObjectsWithoutHash(ctx, 10, 0)
	if len(unhashed) != 1 {
		t.Errorf("expected 1 unhashed, got %d", len(unhashed))
	}

	// GetRandomHashedObjects should return the hashed one
	hashed, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, []string{"backend-a"})
	if err != nil {
		t.Fatalf("GetRandomHashedObjects: %v", err)
	}
	if len(hashed) != 1 {
		t.Errorf("expected 1 hashed, got %d", len(hashed))
	}
}

// -------------------------------------------------------------------------
// DIRECTORY LISTING
// -------------------------------------------------------------------------

// TestListDirectoryChildren verifies the list directory children contract.
// Asserts that ListDirectoryChildren:.
func TestListDirectoryChildren(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/file.txt", "backend-a", 100)
	mustRecordObject(t, s, "bucket/dir/a.txt", "backend-a", 200)
	mustRecordObject(t, s, "bucket/dir/b.txt", "backend-a", 300)

	result, err := s.ListDirectoryChildren(ctx, "bucket/", "", 100)
	if err != nil {
		t.Fatalf("ListDirectoryChildren: %v", err)
	}

	if len(result.Entries) < 2 {
		t.Fatalf("expected at least 2 entries (dir + file), got %d", len(result.Entries))
	}

	// Should have a directory entry "dir/" and a file entry "file.txt"
	var foundDir, foundFile bool
	for _, e := range result.Entries {
		if e.Name == "bucket/dir/" && e.IsDir {
			foundDir = true
			if e.FileCount != 2 {
				t.Errorf("dir file_count = %d, want 2", e.FileCount)
			}
		}
		if e.Name == "bucket/file.txt" && !e.IsDir {
			foundFile = true
		}
	}
	if !foundDir {
		t.Error("missing directory entry for dir/")
	}
	if !foundFile {
		t.Error("missing file entry for file.txt")
	}
}

// TestListDirectoryChildren_UnderscorePrefix guards the SQLite directory query
// against the LIKE-escaping offset bug that affected the Postgres side: the
// caller escapes '_' to '\_', and reusing that escaped length for the
// child-name substring offset would mangle and drop every child. A prefix
// containing an underscore must still list its file and subdirectory children.
// SQLite already passes the unescaped prefix length here; this keeps the two
// implementations in parity.
func TestListDirectoryChildren_UnderscorePrefix(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "dir_test/file.txt", "backend-a", 100)
	mustRecordObject(t, s, "dir_test/sub/inner.txt", "backend-a", 50)

	result, err := s.ListDirectoryChildren(ctx, "dir_test/", "", 100)
	if err != nil {
		t.Fatalf("ListDirectoryChildren: %v", err)
	}

	if file := findEntry(result.Entries, "dir_test/file.txt"); file == nil || file.IsDir {
		t.Errorf("missing file child under underscore prefix; got %+v", result.Entries)
	}
	if dir := findEntry(result.Entries, "dir_test/sub/"); dir == nil || !dir.IsDir {
		t.Errorf("missing subdirectory child under underscore prefix; got %+v", result.Entries)
	}
}

// seedReplicationFixture creates a 2-replica object plus a single-copy
// object under "bucket/" so the replication-aware listing tests share a
// stable fixture. Physical bytes for the bucket/ roll-up is
// (100 + 100) + 50 = 250; logical file count is 2.
func seedReplicationFixture(t *testing.T, s *Store) {
	t.Helper()
	mustRecordObject(t, s, "bucket/replicated.txt", "backend-a", 100)
	mustRecordReplica(t, s, "bucket/replicated.txt", "backend-b", "backend-a", 100)
	mustRecordObject(t, s, "bucket/single.txt", "backend-a", 50)
}

// findEntry returns the first DirEntry matching name; nil when absent.
func findEntry(entries []core.DirEntry, name string) *core.DirEntry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}

// TestListDirectoryChildren_FileRowReplicated asserts that a replicated
// file's row reports logical (single replica) size and the full sorted
// backend set the object lives on.
func TestListDirectoryChildren_FileRowReplicated(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedReplicationFixture(t, s)

	result, err := s.ListDirectoryChildren(context.Background(), "bucket/", "", 100)
	if err != nil {
		t.Fatalf("ListDirectoryChildren: %v", err)
	}
	got := findEntry(result.Entries, "bucket/replicated.txt")
	if got == nil {
		t.Fatal("missing entry for replicated.txt")
	}
	if got.TotalSize != 100 {
		t.Errorf("TotalSize = %d, want 100 (logical)", got.TotalSize)
	}
	if want := []string{"backend-a", "backend-b"}; !reflect.DeepEqual(got.Backends, want) {
		t.Errorf("Backends = %v, want %v (sorted)", got.Backends, want)
	}
}

// TestListDirectoryChildren_FileRowSingle asserts that a single-copy
// object reports its lone backend in a one-element slice.
func TestListDirectoryChildren_FileRowSingle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedReplicationFixture(t, s)

	result, err := s.ListDirectoryChildren(context.Background(), "bucket/", "", 100)
	if err != nil {
		t.Fatalf("ListDirectoryChildren: %v", err)
	}
	got := findEntry(result.Entries, "bucket/single.txt")
	if got == nil {
		t.Fatal("missing entry for single.txt")
	}
	if got.TotalSize != 50 {
		t.Errorf("TotalSize = %d, want 50", got.TotalSize)
	}
	if want := []string{"backend-a"}; !reflect.DeepEqual(got.Backends, want) {
		t.Errorf("Backends = %v, want %v", got.Backends, want)
	}
}

// TestListDirectoryChildren_DirRollupPhysicalBytes asserts that a
// directory roll-up sums physical bytes across every replica row and
// counts distinct object keys for FileCount.
func TestListDirectoryChildren_DirRollupPhysicalBytes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedReplicationFixture(t, s)

	root, err := s.ListDirectoryChildren(context.Background(), "", "", 100)
	if err != nil {
		t.Fatalf("ListDirectoryChildren: %v", err)
	}
	got := findEntry(root.Entries, "bucket/")
	if got == nil || !got.IsDir {
		t.Fatal("missing directory entry for bucket/ at root")
	}
	if got.FileCount != 2 {
		t.Errorf("FileCount = %d, want 2 (distinct object keys)", got.FileCount)
	}
	if got.TotalSize != 250 {
		t.Errorf("TotalSize = %d, want 250 (physical: 100+100+50)", got.TotalSize)
	}
}

// -------------------------------------------------------------------------
// LIFECYCLE (EXPIRATION)
// -------------------------------------------------------------------------

// TestListExpiredObjects verifies the list expired objects contract.
// Asserts that ListExpiredObjects:.
func TestListExpiredObjects(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/old", "backend-a", 100)
	mustRecordObject(t, s, "bucket/new", "backend-a", 200)

	// Everything is "new" (just created)  -  none should be expired
	cutoff := time.Now().Add(-time.Hour)
	expired, err := s.ListExpiredObjects(ctx, core.ExpiredObjectsQuery{
		Prefix: "bucket/", Cutoff: cutoff, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListExpiredObjects: %v", err)
	}
	if len(expired) != 0 {
		t.Errorf("expected 0 expired, got %d", len(expired))
	}

	// Use a future cutoff  -  everything should be expired
	expired, _ = s.ListExpiredObjects(ctx, core.ExpiredObjectsQuery{
		Prefix: "bucket/", Cutoff: time.Now().Add(time.Hour), Limit: 10,
	})
	if len(expired) != 2 {
		t.Errorf("expected 2 expired with future cutoff, got %d", len(expired))
	}
}

// expiredKeys runs one query and returns the keys it selected, sorted, so a
// test can compare against a literal without depending on row order.
func expiredKeys(t *testing.T, s *Store, q core.ExpiredObjectsQuery) []string {
	t.Helper()
	rows, err := s.ListExpiredObjects(context.Background(), q)
	if err != nil {
		t.Fatalf("ListExpiredObjects: %v", err)
	}
	keys := make([]string, 0, len(rows))
	for i := range rows {
		keys = append(keys, rows[i].ObjectKey)
	}
	slices.Sort(keys)
	return keys
}

// TestListExpiredObjects_TagFilter verifies that a tag filter selects the
// intersection: an object has to carry every tag asked for, matching on the
// value and not the key alone.
func TestListExpiredObjects_TagFilter(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	for _, key := range []string{"bucket/both", "bucket/one", "bucket/other", "bucket/none"} {
		mustRecordObject(t, s, key, "backend-a", 100)
	}
	tagged := map[string][]core.Tag{
		"bucket/both":  {{Key: "env", Value: "staging"}, {Key: "team", Value: "infra"}},
		"bucket/one":   {{Key: "env", Value: "staging"}},
		"bucket/other": {{Key: "env", Value: "prod"}, {Key: "team", Value: "infra"}},
	}
	for key, tags := range tagged {
		if err := s.ReplaceObjectTags(ctx, key, tags); err != nil {
			t.Fatalf("tag %s: %v", key, err)
		}
	}

	future := time.Now().Add(time.Hour)
	base := core.ExpiredObjectsQuery{Prefix: "bucket/", Cutoff: future, Limit: 10}

	t.Run("no filter selects every object", func(t *testing.T) {
		got := expiredKeys(t, s, base)
		if len(got) != 4 {
			t.Errorf("got %v, want all four keys", got)
		}
	})

	t.Run("one tag", func(t *testing.T) {
		q := base
		q.Tags = map[string]string{"env": "staging"}
		got := expiredKeys(t, s, q)
		want := []string{"bucket/both", "bucket/one"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("two tags select the intersection", func(t *testing.T) {
		q := base
		q.Tags = map[string]string{"env": "staging", "team": "infra"}
		got := expiredKeys(t, s, q)
		want := []string{"bucket/both"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("value must match, not just the key", func(t *testing.T) {
		q := base
		q.Tags = map[string]string{"env": "nonexistent"}
		if got := expiredKeys(t, s, q); len(got) != 0 {
			t.Errorf("got %v, want nothing", got)
		}
	})

	t.Run("cutoff still applies alongside the tags", func(t *testing.T) {
		q := base
		q.Tags = map[string]string{"env": "staging"}
		q.Cutoff = time.Now().Add(-time.Hour)
		if got := expiredKeys(t, s, q); len(got) != 0 {
			t.Errorf("got %v, want nothing for a past cutoff", got)
		}
	})
}

// -------------------------------------------------------------------------
// ADMIN STORE - ENCRYPTION OPERATIONS
// -------------------------------------------------------------------------

// TestEncryptionAdmin_MarkAndList verifies marking objects for re-encryption and listing them.
func TestEncryptionAdmin_MarkAndList(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/plain", "backend-a", 1024)

	// List unencrypted
	unenc, err := s.ListUnencryptedLocations(ctx, 10, core.Cursor{})
	if err != nil {
		t.Fatalf("ListUnencryptedLocations: %v", err)
	}
	if len(unenc) != 1 {
		t.Errorf("expected 1 unencrypted, got %d", len(unenc))
	}

	// Mark encrypted
	if err := s.MarkObjectEncrypted(ctx, "bucket/plain", "backend-a", []byte("dek"), "key-1", 1024, 1100); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}

	// List encrypted
	enc, err := s.ListEncryptedLocations(ctx, "key-1", 10, 0)
	if err != nil {
		t.Fatalf("ListEncryptedLocations: %v", err)
	}
	if len(enc) != 1 {
		t.Errorf("expected 1 encrypted, got %d", len(enc))
	}

	// Update encryption key (rotation)
	if err := s.UpdateEncryptionKey(ctx, "bucket/plain", "backend-a", []byte("new-dek"), "key-2"); err != nil {
		t.Fatalf("UpdateEncryptionKey: %v", err)
	}

	// Old key should have 0 entries
	enc, _ = s.ListEncryptedLocations(ctx, "key-1", 10, 0)
	if len(enc) != 0 {
		t.Errorf("expected 0 for old key, got %d", len(enc))
	}

	// Decrypt
	if err := s.MarkObjectDecrypted(ctx, "bucket/plain", "backend-a", 1024); err != nil {
		t.Fatalf("MarkObjectDecrypted: %v", err)
	}

	locs, _ := s.GetAllObjectLocations(ctx, "bucket/plain")
	if locs[0].Encrypted {
		t.Error("expected Encrypted=false after MarkObjectDecrypted")
	}
}

// -------------------------------------------------------------------------
// ADMIN STORE - NOTIFICATION OUTBOX
// -------------------------------------------------------------------------

// TestNotificationOutbox_Lifecycle verifies insert, query, and delivery of notification events.
func TestNotificationOutbox_Lifecycle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertNotification(ctx, "s3:ObjectCreated:Put", `{"key":"bucket/a"}`, "https://hook.example.com"); err != nil {
		t.Fatalf("InsertNotification: %v", err)
	}

	pending, err := s.GetPendingNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingNotifications: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}
	if pending[0].EventType != "s3:ObjectCreated:Put" {
		t.Errorf("event_type = %q", pending[0].EventType)
	}

	// Complete
	if err := s.CompleteNotification(ctx, pending[0].ID); err != nil {
		t.Fatalf("CompleteNotification: %v", err)
	}

	pending, _ = s.GetPendingNotifications(ctx, 10)
	if len(pending) != 0 {
		t.Errorf("expected 0 after complete, got %d", len(pending))
	}
}

// TestNotificationOutbox_Retry verifies the notification outbox retry contract.
// Asserts that RetryNotification:.
func TestNotificationOutbox_Retry(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustInsertNotification(t, s, "test", `{}`, "https://hook.example.com")

	pending, _ := s.GetPendingNotifications(ctx, 10)
	if err := s.RetryNotification(ctx, pending[0].ID, time.Hour, "timeout"); err != nil {
		t.Fatalf("RetryNotification: %v", err)
	}

	// Should not be pending (next_retry in the future)
	pending, _ = s.GetPendingNotifications(ctx, 10)
	if len(pending) != 0 {
		t.Errorf("expected 0 pending after retry, got %d", len(pending))
	}
}

// -------------------------------------------------------------------------
// ADVISORY LOCK
// -------------------------------------------------------------------------

// TestWithAdvisoryLock verifies the with advisory lock contract.
// Asserts that WithAdvisoryLock:.
func TestWithAdvisoryLock(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	called := false
	acquired, err := s.WithAdvisoryLock(ctx, 1001, func(ctx context.Context) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithAdvisoryLock: %v", err)
	}
	if !acquired {
		t.Error("expected lock to be acquired")
	}
	if !called {
		t.Error("callback was not called")
	}
}

// TestWithAdvisoryLock_PropagatesContext verifies that the caller's context
// (including cancellation) is forwarded to the callback, not discarded.
func TestWithAdvisoryLock_PropagatesContext(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	acquired, err := s.WithAdvisoryLock(ctx, 1001, func(ctx context.Context) error {
		if ctx.Err() == nil {
			t.Error("expected cancelled context inside callback, got non-cancelled")
		}
		return ctx.Err()
	})
	if !acquired {
		t.Error("expected lock to be acquired")
	}
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}

// TestWithAdvisoryLock_DeadlinePropagated verifies that a context deadline
// set by the caller is visible inside the advisory lock callback.
func TestWithAdvisoryLock_DeadlinePropagated(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	deadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	acquired, err := s.WithAdvisoryLock(ctx, 1001, func(ctx context.Context) error {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("expected deadline in callback context, got none")
			return nil
		}
		if !dl.Equal(deadline) {
			t.Errorf("deadline = %v, want %v", dl, deadline)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithAdvisoryLock: %v", err)
	}
	if !acquired {
		t.Error("expected lock to be acquired")
	}
}

// -------------------------------------------------------------------------
// BACKEND LIFECYCLE
// -------------------------------------------------------------------------

// TestDeleteBackendData verifies the delete backend data contract.
// Asserts that DeleteBackendData:.
func TestDeleteBackendData(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/b", "backend-a", 200)

	if err := s.DeleteBackendData(ctx, "backend-a"); err != nil {
		t.Fatalf("DeleteBackendData: %v", err)
	}

	count, _, _ := s.BackendObjectStats(ctx, "backend-a")
	if count != 0 {
		t.Errorf("expected 0 objects after delete, got %d", count)
	}
}

// -------------------------------------------------------------------------
// SCHEMA VERSION
// -------------------------------------------------------------------------

// TestVerifySchemaVersion verifies the verify schema version contract.
// Asserts that VerifySchemaVersion:.
func TestVerifySchemaVersion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.VerifySchemaVersion(ctx); err != nil {
		t.Errorf("VerifySchemaVersion: %v", err)
	}
}

// TestCorruptTimestamp_GetAllObjectLocations verifies that a malformed
// created_at timestamp in object_locations returns an error instead of
// silently defaulting to the zero time.
func TestCorruptTimestamp_GetAllObjectLocations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/bad-ts", "backend-a", 100)

	_, err := s.db.ExecContext(ctx, `UPDATE object_locations SET created_at = 'not-a-date' WHERE object_key = 'bucket/bad-ts'`)
	if err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}

	_, err = s.GetAllObjectLocations(ctx, "bucket/bad-ts")
	if err == nil {
		t.Fatal("expected error from corrupt timestamp, got nil")
	}
	if !strings.Contains(err.Error(), "invalid created_at timestamp") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestCorruptTimestamp_ListObjects verifies that a malformed created_at
// in object_locations surfaces as an error from ListObjects.
func TestCorruptTimestamp_ListObjects(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/bad-ts2", "backend-a", 100)

	_, err := s.db.ExecContext(ctx, `UPDATE object_locations SET created_at = 'garbage' WHERE object_key = 'bucket/bad-ts2'`)
	if err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}

	_, err = s.ListObjects(ctx, "bucket/", "", 100)
	if err == nil {
		t.Fatal("expected error from corrupt timestamp, got nil")
	}
}

// TestCorruptTimestamp_MultipartUpload verifies that a malformed created_at
// in multipart_uploads surfaces as an error from GetMultipartUpload.
func TestCorruptTimestamp_MultipartUpload(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateUpload(t, s, "upload-1", "bucket/mp-key", "backend-a")

	_, err := s.db.ExecContext(ctx, `UPDATE multipart_uploads SET created_at = 'bad' WHERE upload_id = 'upload-1'`)
	if err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}

	_, err = s.GetMultipartUpload(ctx, "upload-1")
	if err == nil {
		t.Fatal("expected error from corrupt multipart timestamp, got nil")
	}
}

// TestCorruptTimestamp_GetParts verifies that a malformed created_at in
// multipart_parts surfaces as an error from GetParts.
func TestCorruptTimestamp_GetParts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateUpload(t, s, "up-parts", "bucket/mp", "backend-a")
	if err := s.RecordPart(ctx, "up-parts", 1, "etag1", 100, nil); err != nil {
		t.Fatalf("RecordPart: %v", err)
	}

	_, err := s.db.ExecContext(ctx, `UPDATE multipart_parts SET created_at = 'corrupt' WHERE upload_id = 'up-parts'`)
	if err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}

	_, err = s.GetParts(ctx, "up-parts")
	if err == nil {
		t.Fatal("expected error from corrupt part timestamp, got nil")
	}
}

// TestCorruptTimestamp_ListMultipartUploads verifies that a malformed
// created_at in multipart_uploads surfaces as an error from ListMultipartUploads.
func TestCorruptTimestamp_ListMultipartUploads(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateUpload(t, s, "up-list", "bucket/mp-list", "backend-a")

	_, err := s.db.ExecContext(ctx, `UPDATE multipart_uploads SET created_at = 'corrupt' WHERE upload_id = 'up-list'`)
	if err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}

	_, err = s.ListMultipartUploads(ctx, "bucket/", 100)
	if err == nil {
		t.Fatal("expected error from corrupt multipart upload timestamp, got nil")
	}
}

// TestCorruptTimestamp_GetStaleMultipartUploads verifies that a malformed
// created_at surfaces as an error from GetStaleMultipartUploads (used by
// the scanMultipartUploads helper).
func TestCorruptTimestamp_GetStaleMultipartUploads(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateUpload(t, s, "up-stale", "bucket/stale-key", "backend-a")

	// '0bad' sorts before any RFC3339 timestamp (starts with '2') so the
	// WHERE created_at < ? filter includes the corrupt row.
	_, err := s.db.ExecContext(ctx, `UPDATE multipart_uploads SET created_at = '0bad' WHERE upload_id = 'up-stale'`)
	if err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}

	_, err = s.GetStaleMultipartUploads(ctx, 0)
	if err == nil {
		t.Fatal("expected error from corrupt stale multipart timestamp, got nil")
	}
}

// TestCorruptTimestamp_ListExpiredObjects verifies that a malformed created_at
// surfaces as an error from ListExpiredObjects.
func TestCorruptTimestamp_ListExpiredObjects(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/expired", "backend-a", 100)

	// '0bad' sorts before any RFC3339 timestamp so the WHERE created_at < ?
	// filter includes the corrupt row.
	_, err := s.db.ExecContext(ctx, `UPDATE object_locations SET created_at = '0bad' WHERE object_key = 'bucket/expired'`)
	if err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}

	_, err = s.ListExpiredObjects(ctx, core.ExpiredObjectsQuery{
		Prefix: "bucket/", Cutoff: time.Now().Add(time.Hour), Limit: 100,
	})
	if err == nil {
		t.Fatal("expected error from corrupt expired object timestamp, got nil")
	}
}

// TestCorruptTimestamp_ListObjectsByBackend verifies that a malformed
// created_at surfaces as an error from ListObjectsByBackend.
func TestCorruptTimestamp_ListObjectsByBackend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/by-be", "backend-a", 100)

	_, err := s.db.ExecContext(ctx, `UPDATE object_locations SET created_at = 'bad' WHERE object_key = 'bucket/by-be'`)
	if err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}

	_, err = s.ListObjectsByBackend(ctx, "backend-a", 100)
	if err == nil {
		t.Fatal("expected error from corrupt timestamp, got nil")
	}
}

// TestCorruptTimestamp_GetQuotaStats verifies that a malformed updated_at
// in backend_quotas surfaces as an error from GetQuotaStats.
func TestCorruptTimestamp_GetQuotaStats(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.db.ExecContext(ctx, `UPDATE backend_quotas SET updated_at = 'bad' WHERE backend_name = 'backend-a'`)
	if err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}

	_, err = s.GetQuotaStats(ctx)
	if err == nil {
		t.Fatal("expected error from corrupt quota timestamp, got nil")
	}
}

// TestCorruptTimestamp_GetUnderReplicatedObjects verifies that a malformed
// created_at surfaces as an error from the replication query path
// (scanObjectLocations helper).
func TestCorruptTimestamp_GetUnderReplicatedObjects(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/under-rep", "backend-a", 100)

	_, err := s.db.ExecContext(ctx, `UPDATE object_locations SET created_at = 'bad' WHERE object_key = 'bucket/under-rep'`)
	if err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}

	_, err = s.GetUnderReplicatedObjects(ctx, 2, 100)
	if err == nil {
		t.Fatal("expected error from corrupt replication timestamp, got nil")
	}
}

// TestVerifySchemaVersion_NewerThanExpected verifies that a database with a
// schema version newer than the binary expects returns an error, preventing
// silent data corruption on binary downgrades.
func TestVerifySchemaVersion_NewerThanExpected(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.db.ExecContext(ctx, `UPDATE schema_version SET version = version + 100`)
	if err != nil {
		t.Fatalf("bump schema version: %v", err)
	}

	err = s.VerifySchemaVersion(ctx)
	if err == nil {
		t.Fatal("expected error for schema newer than expected, got nil")
	}
	if !strings.Contains(err.Error(), "newer than expected") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestVerifySchemaVersion_OlderThanExpected verifies that a database whose
// schema version is older than this binary expects surfaces the
// "older than expected" diagnostic so partial-migration failures cannot
// be silently ignored at startup.
func TestVerifySchemaVersion_OlderThanExpected(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	// expectedSchemaVersion is 2, so 1 is older.
	if _, err := s.db.ExecContext(ctx, `UPDATE schema_version SET version = 1`); err != nil {
		t.Fatalf("downgrade schema version: %v", err)
	}

	err := s.VerifySchemaVersion(ctx)
	if err == nil {
		t.Fatal("expected error for schema older than expected, got nil")
	}
	if !strings.Contains(err.Error(), "older than expected") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestVerifySchemaVersion_TableMissing verifies that VerifySchemaVersion
// returns the "schema_version table does not exist" diagnostic when the
// database has not been migrated. Distinguishing "uninitialised" from
// "older than expected" lets startup logs point operators at the right
// remediation step.
func TestVerifySchemaVersion_TableMissing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.db.ExecContext(ctx, `DROP TABLE schema_version`); err != nil {
		t.Fatalf("drop schema_version: %v", err)
	}

	err := s.VerifySchemaVersion(ctx)
	if err == nil {
		t.Fatal("expected error when schema_version table is missing, got nil")
	}
	if !strings.Contains(err.Error(), "schema_version table does not exist") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestRunMigrations_NewerSchemaReturnsError covers the one mismatch that is
// still fatal. A database written by a later release may contain changes this
// binary knows nothing about, so running against it is refused rather than
// guessed at.
func TestRunMigrations_NewerSchemaReturnsError(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.db.ExecContext(ctx, `UPDATE schema_version SET version = version + 100`); err != nil {
		t.Fatalf("bump schema version: %v", err)
	}

	err := s.RunMigrations(ctx)
	if err == nil {
		t.Fatal("expected an error for a schema newer than the binary, got nil")
	}
	if !strings.Contains(err.Error(), "newer than expected") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestRunMigrations_UpgradesAnOlderDatabase is the behaviour the numbered
// migrations exist for: a database left at an earlier version is brought up to
// the current one rather than refused. Before this, any mismatch was fatal and
// every schema change demanded hand-migration by the operator.
func TestRunMigrations_UpgradesAnOlderDatabase(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	// Rewind to the version before the newest migration, as an installation
	// predating it would be.
	rewindToSchemaVersion(t, s, expectedSchemaVersion-1)

	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("RunMigrations on an older database: %v", err)
	}

	version, exists, err := s.currentSchemaVersion(ctx)
	if err != nil || !exists {
		t.Fatalf("read schema version: %v (exists=%v)", err, exists)
	}
	if version != expectedSchemaVersion {
		t.Errorf("schema version = %d after upgrade, want %d", version, expectedSchemaVersion)
	}

	// Running again is a no-op: an applied migration is recorded, so it is not
	// replayed on the next start.
	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("second RunMigrations: %v", err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_version WHERE version = ?`, expectedSchemaVersion).Scan(&rows); err != nil {
		t.Fatalf("count version rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("version %d recorded %d times, want once", expectedSchemaVersion, rows)
	}
}

// TestMigrations_AreNumberedAndReachExpectedVersion guards the pairing between
// the embedded files and expectedSchemaVersion. A migration added without
// bumping the constant would never run; a constant bumped without a migration
// would leave every database reporting a version it never reached.
func TestMigrations_AreNumberedAndReachExpectedVersion(t *testing.T) {
	t.Parallel()

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no embedded migrations found")
	}

	for i, m := range migrations {
		if i > 0 && m.version <= migrations[i-1].version {
			t.Errorf("migration %d (%s) does not sort after %d", m.version, m.name, migrations[i-1].version)
		}
		if m.version > expectedSchemaVersion {
			t.Errorf("migration %d (%s) is above expectedSchemaVersion %d", m.version, m.name, expectedSchemaVersion)
		}
	}
	if highest := migrations[len(migrations)-1].version; highest != expectedSchemaVersion {
		t.Errorf("highest migration is %d but expectedSchemaVersion is %d", highest, expectedSchemaVersion)
	}
}

// -------------------------------------------------------------------------
// ADDITIONAL COVERAGE
// -------------------------------------------------------------------------

// TestDeleteObjectLocation verifies the delete object location contract.
// Asserts that DeleteObjectLocation:.
func TestDeleteObjectLocation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/key1", "backend-a", 100)
	mustRecordReplica(t, s, "bucket/key1", "backend-b", "backend-a", 100)

	if stats, _ := s.GetQuotaStats(ctx); stats["backend-b"].BytesUsed != 100 {
		t.Fatalf("backend-b bytes_used before delete = %d, want 100", stats["backend-b"].BytesUsed)
	}

	if err := s.DeleteObjectLocation(ctx, "bucket/key1", "backend-b"); err != nil {
		t.Fatalf("DeleteObjectLocation: %v", err)
	}

	locs, _ := s.GetAllObjectLocations(ctx, "bucket/key1")
	if len(locs) != 1 || locs[0].BackendName != "backend-a" {
		t.Errorf("expected only backend-a, got %+v", locs)
	}

	// The removed copy's bytes must be debited so bytes_used stays equal to
	// SUM(object_locations.size_bytes) - the ledger invariant #1084 broke.
	if stats, _ := s.GetQuotaStats(ctx); stats["backend-b"].BytesUsed != 0 {
		t.Errorf("backend-b bytes_used after delete = %d, want 0", stats["backend-b"].BytesUsed)
	}

	// With the counter kept honest, usage-reconcile finds no drift to correct.
	adjustments, err := s.ReconcileUsage(ctx)
	if err != nil {
		t.Fatalf("ReconcileUsage: %v", err)
	}
	if len(adjustments) != 0 {
		t.Errorf("reconcile should find no drift, got %v", adjustments)
	}

	// Deleting a copy that is already gone is a benign no-op: no error and no
	// spurious debit against the backend that still holds the object.
	if err := s.DeleteObjectLocation(ctx, "bucket/key1", "backend-b"); err != nil {
		t.Fatalf("DeleteObjectLocation (already gone): %v", err)
	}
	if stats, _ := s.GetQuotaStats(ctx); stats["backend-a"].BytesUsed != 100 {
		t.Errorf("backend-a bytes_used after no-op delete = %d, want 100", stats["backend-a"].BytesUsed)
	}
}

// TestGetObjectCounts verifies per-backend object count aggregation.
func TestGetObjectCounts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/b", "backend-a", 200)
	mustRecordObject(t, s, "bucket/c", "backend-b", 300)

	counts, err := s.GetObjectCounts(ctx)
	if err != nil {
		t.Fatalf("GetObjectCounts: %v", err)
	}
	if counts["backend-a"] != 2 {
		t.Errorf("backend-a count = %d, want 2", counts["backend-a"])
	}
	if counts["backend-b"] != 1 {
		t.Errorf("backend-b count = %d, want 1", counts["backend-b"])
	}
}

// TestGetUnverifiedObjectCounts pins the per-backend NULL-content_hash
// aggregation that drives the dashboard's "Unverified" column (#405).
// Records two objects with hashes and one without, asserts the count
// per backend matches the NULL population.
func TestGetUnverifiedObjectCounts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	// One object on backend-a has no content hash (NULL); one does.
	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	hashed := &core.StoredForm{ContentHash: "deadbeef"}
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/b", Backend: "backend-a", Size: 200, Form: hashed}); err != nil {
		t.Fatalf("RecordObject hashed: %v", err)
	}
	// One object on backend-b has no content hash.
	mustRecordObject(t, s, "bucket/c", "backend-b", 300)

	counts, err := s.GetUnverifiedObjectCounts(ctx)
	if err != nil {
		t.Fatalf("GetUnverifiedObjectCounts: %v", err)
	}
	if counts["backend-a"] != 1 {
		t.Errorf("backend-a unverified = %d, want 1", counts["backend-a"])
	}
	if counts["backend-b"] != 1 {
		t.Errorf("backend-b unverified = %d, want 1", counts["backend-b"])
	}
}

// TestGetStaleMultipartUploads verifies the get stale multipart uploads contract.
// Asserts that GetStaleMultipartUploads:.
func TestGetStaleMultipartUploads(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateUpload(t, s, "u1", "bucket/a", "backend-a")

	// Nothing stale with a long threshold
	stale, err := s.GetStaleMultipartUploads(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("GetStaleMultipartUploads: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("expected 0 stale, got %d", len(stale))
	}

	// Everything stale with zero threshold
	stale, err = s.GetStaleMultipartUploads(ctx, 0)
	if err != nil {
		t.Fatalf("GetStaleMultipartUploads zero: %v", err)
	}
	if len(stale) != 1 {
		t.Errorf("expected 1 stale, got %d", len(stale))
	}
}

// TestGetMultipartUploadsByBackend verifies the get multipart uploads by backend contract.
// Asserts that GetMultipartUploadsByBackend:.
func TestGetMultipartUploadsByBackend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateUpload(t, s, "u1", "bucket/a", "backend-a")
	mustCreateUpload(t, s, "u2", "bucket/b", "backend-b")

	uploads, err := s.GetMultipartUploadsByBackend(ctx, "backend-a")
	if err != nil {
		t.Fatalf("GetMultipartUploadsByBackend: %v", err)
	}
	if len(uploads) != 1 {
		t.Errorf("expected 1, got %d", len(uploads))
	}
	if uploads[0].UploadID != "u1" {
		t.Errorf("upload_id = %q, want u1", uploads[0].UploadID)
	}
}

// TestGetUnderReplicatedObjectsExcluding verifies under-replication detection with backend exclusions.
func TestGetUnderReplicatedObjectsExcluding(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/b", "backend-b", 200)

	// Both under-replicated at factor 2
	under, err := s.GetUnderReplicatedObjectsExcluding(ctx, 2, 10, []string{"backend-b"})
	if err != nil {
		t.Fatalf("GetUnderReplicatedObjectsExcluding: %v", err)
	}

	// Only bucket/a should appear (backend-b is excluded)
	for _, loc := range under {
		if loc.BackendName == "backend-b" {
			t.Errorf("excluded backend-b should not appear in results")
		}
	}
}

// TestCountOverReplicatedObjects verifies the count over replicated objects contract.
// Asserts that CountOverReplicatedObjects:.
func TestCountOverReplicatedObjects(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/key1", "backend-a", 100)
	mustRecordReplica(t, s, "bucket/key1", "backend-b", "backend-a", 100)

	count, err := s.CountOverReplicatedObjects(ctx, 1)
	if err != nil {
		t.Fatalf("CountOverReplicatedObjects: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}

	count, _ = s.CountOverReplicatedObjects(ctx, 2)
	if count != 0 {
		t.Errorf("count at factor 2 = %d, want 0", count)
	}
}

// TestGetObjectBackendsForKeys_EmptyInput verifies the helper returns
// an empty map for a nil/empty key slice without issuing a query.
func TestGetObjectBackendsForKeys_EmptyInput(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	got, err := s.GetObjectBackendsForKeys(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetObjectBackendsForKeys(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map for nil input, got %v", got)
	}
	got, err = s.GetObjectBackendsForKeys(context.Background(), []string{})
	if err != nil {
		t.Fatalf("GetObjectBackendsForKeys([]): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map for empty slice, got %v", got)
	}
}

// TestGetObjectBackendsForKeys_GroupsByKey verifies replicas of the
// same key are bucketed together and unrelated keys are absent.
func TestGetObjectBackendsForKeys_GroupsByKey(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecordObject(t, s, "bucket/k1", "backend-a", 100)
	mustRecordReplica(t, s, "bucket/k1", "backend-b", "backend-a", 100)
	mustRecordObject(t, s, "bucket/k2", "backend-a", 50)

	got, err := s.GetObjectBackendsForKeys(ctx, []string{"bucket/k1", "bucket/k2", "bucket/missing"})
	if err != nil {
		t.Fatalf("GetObjectBackendsForKeys: %v", err)
	}
	if len(got["bucket/k1"]) != 2 {
		t.Errorf("bucket/k1 should have 2 backends, got %v", got["bucket/k1"])
	}
	if len(got["bucket/k2"]) != 1 || got["bucket/k2"][0] != "backend-a" {
		t.Errorf("bucket/k2 backends mismatch: %v", got["bucket/k2"])
	}
	if _, ok := got["bucket/missing"]; ok {
		t.Errorf("missing key should not be in result map: %v", got)
	}
}

// TestDeleteObjectsBatch_EmptyInput verifies the batch helper returns
// an empty map for nil/empty input without opening a transaction.
func TestDeleteObjectsBatch_EmptyInput(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	got, err := s.DeleteObjectsBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("DeleteObjectsBatch(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

// TestDeleteObjectsBatch_RemovesRowsAndDecrementsQuotas verifies a
// mixed batch removes every supplied key's rows, decrements the
// affected backend quotas exactly once each by the summed sizes, and
// returns the per-key displaced copies for cleanup.
func TestDeleteObjectsBatch_RemovesRowsAndDecrementsQuotas(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecordObject(t, s, "bucket/k1", "backend-a", 100)
	mustRecordReplica(t, s, "bucket/k1", "backend-b", "backend-a", 100)
	mustRecordObject(t, s, "bucket/k2", "backend-a", 50)

	got, err := s.DeleteObjectsBatch(ctx, []string{"bucket/k1", "bucket/k2", "bucket/missing"})
	if err != nil {
		t.Fatalf("DeleteObjectsBatch: %v", err)
	}
	if len(got["bucket/k1"]) != 2 {
		t.Errorf("k1 should have 2 displaced copies, got %v", got["bucket/k1"])
	}
	if len(got["bucket/k2"]) != 1 || got["bucket/k2"][0].BackendName != "backend-a" {
		t.Errorf("k2 displaced copy mismatch: %v", got["bucket/k2"])
	}
	if _, ok := got["bucket/missing"]; ok {
		t.Errorf("missing key must not be in result map: %v", got)
	}

	// Verify rows are gone.
	for _, k := range []string{"bucket/k1", "bucket/k2"} {
		if _, err := s.GetAllObjectLocations(ctx, k); !errors.Is(err, core.ErrObjectNotFound) {
			t.Errorf("expected %s gone, got err=%v", k, err)
		}
	}

	// Verify quotas decremented by the summed sizes.
	stats, err := s.GetQuotaStats(ctx)
	if err != nil {
		t.Fatalf("GetQuotaStats: %v", err)
	}
	if stats["backend-a"].BytesUsed != 0 {
		t.Errorf("backend-a bytes_used = %d, want 0 (k1+k2 = 150 removed)", stats["backend-a"].BytesUsed)
	}
	if stats["backend-b"].BytesUsed != 0 {
		t.Errorf("backend-b bytes_used = %d, want 0 (k1 replica = 100 removed)", stats["backend-b"].BytesUsed)
	}
}

// TestListAllEncryptedLocations verifies the list all encrypted locations contract.
// Asserts that RecordObject:.
func TestListAllEncryptedLocations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	form := &core.StoredForm{
		Encrypted:     true,
		EncryptionKey: []byte("dek"),
		KeyID:         "key-1",
		PlaintextSize: 1024,
	}
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/enc", Backend: "backend-a", Size: 1100, Form: form}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	locs, err := s.ListAllEncryptedLocations(ctx, 10, core.Cursor{})
	if err != nil {
		t.Fatalf("ListAllEncryptedLocations: %v", err)
	}
	if len(locs) != 1 {
		t.Errorf("expected 1, got %d", len(locs))
	}
	if locs[0].PlaintextSize != 1024 {
		t.Errorf("plaintext_size = %d, want 1024", locs[0].PlaintextSize)
	}
}

// -------------------------------------------------------------------------
// CLEANUP DLQ - END TO END
// -------------------------------------------------------------------------

// TestStore_MoveCleanupToDLQ_GraduatesQueueRow asserts the
// engine-level wrapper (Store.MoveCleanupToDLQ delegating to
// core.MoveCleanupToDLQ over a real Runner) atomically moves the
// queue row into cleanup_dlq, leaves orphan_bytes untouched, and
// preserves enough context (key, size, backend, last_error) for
// operator triage.
func TestStore_MoveCleanupToDLQ_GraduatesQueueRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustEnqueueCleanup(t, s, "backend-a", "bucket/doomed")
	if err := s.IncrementOrphanBytes(ctx, "backend-a", 256); err != nil {
		t.Fatalf("IncrementOrphanBytes: %v", err)
	}

	// Stamp a last_error and look up the queue row id.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE cleanup_queue SET last_error = ? WHERE object_key = ?`,
		"earlier transient", "bucket/doomed",
	); err != nil {
		t.Fatalf("set last_error: %v", err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM cleanup_queue WHERE object_key = ?`, "bucket/doomed",
	).Scan(&id); err != nil {
		t.Fatalf("lookup id: %v", err)
	}

	moved, err := s.MoveCleanupToDLQ(ctx, id, "permanent failure")
	if err != nil {
		t.Fatalf("MoveCleanupToDLQ: %v", err)
	}
	if !moved {
		t.Errorf("expected moved=true on first call")
	}

	// cleanup_queue must be empty for that object.
	var remaining int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cleanup_queue WHERE id = ?`, id,
	).Scan(&remaining); err != nil {
		t.Fatalf("count queue: %v", err)
	}
	if remaining != 0 {
		t.Errorf("cleanup_queue row still present after move; got %d rows", remaining)
	}

	// cleanup_dlq must hold the row with the supplied last_error.
	var (
		dlqOriginal  int64
		dlqBackend   string
		dlqKey       string
		dlqReason    string
		dlqSize      int64
		dlqAttempts  int32
		dlqLastError string
	)
	if err := s.db.QueryRowContext(ctx,
		`SELECT original_id, backend_name, object_key, reason, size_bytes, attempts, COALESCE(last_error, '')
		 FROM cleanup_dlq WHERE original_id = ?`, id,
	).Scan(&dlqOriginal, &dlqBackend, &dlqKey, &dlqReason, &dlqSize, &dlqAttempts, &dlqLastError); err != nil {
		t.Fatalf("probe DLQ: %v", err)
	}
	if dlqOriginal != id || dlqBackend != "backend-a" || dlqKey != "bucket/doomed" ||
		dlqReason != "test" || dlqSize != 256 || dlqLastError != "permanent failure" {
		t.Errorf("DLQ row mismatch: orig=%d backend=%q key=%q reason=%q size=%d err=%q",
			dlqOriginal, dlqBackend, dlqKey, dlqReason, dlqSize, dlqLastError)
	}

	// orphan_bytes must NOT have been decremented - the backend object
	// is still on disk and the DLQ flow is intentionally not a write-off.
	var orphan int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT orphan_bytes FROM backend_quotas WHERE backend_name = ?`, "backend-a",
	).Scan(&orphan); err != nil {
		t.Fatalf("query orphan_bytes: %v", err)
	}
	if orphan != 256 {
		t.Errorf("orphan_bytes=%d, want 256 (move must not decrement)", orphan)
	}
}

// TestStore_MoveCleanupToDLQ_MissingRowReturnsFalse asserts a no-op
// move on a non-existent id - the documented contract for a benign
// concurrent finaliser race.
func TestStore_MoveCleanupToDLQ_MissingRowReturnsFalse(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	moved, err := s.MoveCleanupToDLQ(context.Background(), 99999, "irrelevant")
	if err != nil {
		t.Fatalf("MoveCleanupToDLQ: %v", err)
	}
	if moved {
		t.Errorf("expected moved=false when id does not exist")
	}
}

// TestStore_CleanupDLQDepth_CountsRows asserts the DLQ depth gauge
// query returns the live row count, including across multiple inserts.
func TestStore_CleanupDLQDepth_CountsRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	depth, err := s.CleanupDLQDepth(ctx)
	if err != nil {
		t.Fatalf("CleanupDLQDepth (empty): %v", err)
	}
	if depth != 0 {
		t.Errorf("empty depth=%d, want 0", depth)
	}

	// Seed two queue rows then graduate them both.
	mustEnqueueCleanup(t, s, "backend-a", "bucket/k1")
	mustEnqueueCleanup(t, s, "backend-a", "bucket/k2")
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM cleanup_queue ORDER BY id`)
	if err != nil {
		t.Fatalf("query ids: %v", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.MoveCleanupToDLQ(ctx, id, ""); err != nil {
			t.Fatalf("MoveCleanupToDLQ(%d): %v", id, err)
		}
	}

	depth, err = s.CleanupDLQDepth(ctx)
	if err != nil {
		t.Fatalf("CleanupDLQDepth: %v", err)
	}
	if depth != 2 {
		t.Errorf("depth=%d, want 2", depth)
	}
}

// TestImportObject_UnmanagedCountsForQuotaButNotForWork pins the two halves of
// the managed flag. A stray object sitting outside every configured bucket
// prefix occupies real backend capacity, so placement has to see it in the
// quota totals; but the orchestrator did not put it there, so no worker may
// pick it up and start copying or moving it.
func TestImportObject_UnmanagedCountsForQuotaButNotForWork(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.ImportObject(ctx, "bucket/owned", "backend-a", 100, false, nil); err != nil {
		t.Fatalf("ImportObject(managed): %v", err)
	}
	if _, err := s.ImportObject(ctx, "stray.txt", "backend-a", 400, true, nil); err != nil {
		t.Fatalf("ImportObject(unmanaged): %v", err)
	}

	// Quota counts both: the bytes are on the backend either way.
	count, bytes, err := s.BackendObjectStats(ctx, "backend-a")
	if err != nil {
		t.Fatalf("BackendObjectStats: %v", err)
	}
	if count != 2 || bytes != 500 {
		t.Errorf("stats = %d objects / %d bytes, want 2 / 500", count, bytes)
	}

	// The rebalance, placement and drain candidate scan sees only the owned one.
	movable, err := s.ListObjectsByBackend(ctx, "backend-a", 10)
	if err != nil {
		t.Fatalf("ListObjectsByBackend: %v", err)
	}
	if len(movable) != 1 || movable[0].ObjectKey != "bucket/owned" {
		t.Errorf("movable = %+v, want only bucket/owned", movable)
	}

	// Neither does the replicator (a stray with one copy is not a job), nor
	// checksum backfill, which would spend egress reading the body.
	under, err := s.GetUnderReplicatedObjects(ctx, 2, 10)
	if err != nil {
		t.Fatalf("GetUnderReplicatedObjects: %v", err)
	}
	assertNoStray(t, "replication", under)

	unhashed, err := s.GetObjectsWithoutHash(ctx, 10, 0)
	if err != nil {
		t.Fatalf("GetObjectsWithoutHash: %v", err)
	}
	assertNoStray(t, "checksum backfill", unhashed)
}

// assertNoStray fails when a worker candidate scan surfaced the unmanaged row.
func assertNoStray(t *testing.T, scan string, locs []core.ObjectLocation) {
	t.Helper()
	for i := range locs {
		if locs[i].ObjectKey == "stray.txt" {
			t.Errorf("unmanaged object queued for %s", scan)
		}
	}
}

// TestScrubQueue_OrdersByLeastRecentlyScrubbed verifies the sweep is a queue
// rather than a sample: never-verified copies come first, then the oldest
// stamp, so repeated cycles reach every copy instead of resampling one slice.
func TestScrubQueue_OrdersByLeastRecentlyScrubbed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	for _, key := range []string{"bucket/a", "bucket/b", "bucket/c"} {
		mustRecordObject(t, s, key, "backend-a", 100)
		if err := s.UpdateContentHash(ctx, key, "backend-a", "sha256:"+key); err != nil {
			t.Fatalf("UpdateContentHash(%s): %v", key, err)
		}
	}

	// Stamp two of them, leaving bucket/c never verified.
	if err := s.MarkObjectScrubbed(ctx, "bucket/a", "backend-a"); err != nil {
		t.Fatalf("MarkObjectScrubbed(a): %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := s.MarkObjectScrubbed(ctx, "bucket/b", "backend-a"); err != nil {
		t.Fatalf("MarkObjectScrubbed(b): %v", err)
	}

	got, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, []string{"backend-a"})
	if err != nil {
		t.Fatalf("GetLeastRecentlyScrubbedObjects: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 copies, got %d", len(got))
	}
	want := []string{"bucket/c", "bucket/a", "bucket/b"}
	for i, w := range want {
		if got[i].ObjectKey != w {
			t.Errorf("position %d = %q, want %q (order: %v)", i, got[i].ObjectKey, w, keysOf(got))
		}
	}
}

// TestScrubQueue_FreshWritesDoNotJumpTheQueue is the property that keeps the
// sweep alive on a busy fleet. A never-verified copy written moments ago must
// sort behind an old copy verified long ago; ordering purely on the verified
// timestamp put every new write at the head, and once writes outpaced the
// scrubber nothing older was ever reached.
func TestScrubQueue_FreshWritesDoNotJumpTheQueue(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	for _, key := range []string{"bucket/old", "bucket/fresh"} {
		mustRecordObject(t, s, key, "backend-a", 100)
		if err := s.UpdateContentHash(ctx, key, "backend-a", "sha256:"+key); err != nil {
			t.Fatalf("UpdateContentHash(%s): %v", key, err)
		}
	}

	// bucket/old was written a year ago and verified a month ago.
	// bucket/fresh was written just now and never verified.
	backdate := func(key, created, scrubbed string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx,
			`UPDATE object_locations SET created_at = ?, last_scrubbed_at = ?
			 WHERE object_key = ? AND backend_name = 'backend-a'`,
			created, scrubbed, key,
		); err != nil {
			t.Fatalf("backdating %s: %v", key, err)
		}
	}
	now := time.Now().UTC()
	backdate("bucket/old",
		now.Add(-365*24*time.Hour).Format(time.RFC3339Nano),
		now.Add(-30*24*time.Hour).Format(time.RFC3339Nano))

	got, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, []string{"backend-a"})
	if err != nil {
		t.Fatalf("GetLeastRecentlyScrubbedObjects: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 copies, got %d", len(got))
	}
	if got[0].ObjectKey != "bucket/old" {
		t.Errorf("queue head = %q, want bucket/old: a fresh write must not "+
			"outrank a copy last verified a month ago (order: %v)", got[0].ObjectKey, keysOf(got))
	}
}

// keysOf extracts object keys for a readable ordering assertion failure.
func keysOf(locs []core.ObjectLocation) []string {
	keys := make([]string, len(locs))
	for i := range locs {
		keys[i] = locs[i].ObjectKey
	}
	return keys
}

// TestOldestUnverifiedAge_ReportsCoverage verifies the figures the dashboard
// and the alerting rule read: how stale the oldest verified copy is, and how
// many have never been verified.
func TestOldestUnverifiedAge_ReportsCoverage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	// No hashed copies at all: nothing to report.
	age, never, err := s.OldestUnverifiedAge(ctx)
	if err != nil {
		t.Fatalf("OldestUnverifiedAge on an empty ledger: %v", err)
	}
	if age != 0 || never != 0 {
		t.Errorf("empty ledger reported age=%s never=%d, want 0/0", age, never)
	}

	for _, key := range []string{"bucket/a", "bucket/b"} {
		mustRecordObject(t, s, key, "backend-a", 100)
		if err := s.UpdateContentHash(ctx, key, "backend-a", "sha256:"+key); err != nil {
			t.Fatalf("UpdateContentHash(%s): %v", key, err)
		}
	}

	// Hashed but unverified: both count as never verified.
	_, never, err = s.OldestUnverifiedAge(ctx)
	if err != nil {
		t.Fatalf("OldestUnverifiedAge: %v", err)
	}
	if never != 2 {
		t.Errorf("never verified = %d, want 2", never)
	}

	// Verifying one leaves the other outstanding and starts the age clock.
	if err := s.MarkObjectScrubbed(ctx, "bucket/a", "backend-a"); err != nil {
		t.Fatalf("MarkObjectScrubbed: %v", err)
	}
	age, never, err = s.OldestUnverifiedAge(ctx)
	if err != nil {
		t.Fatalf("OldestUnverifiedAge after stamping: %v", err)
	}
	if never != 1 {
		t.Errorf("never verified = %d, want 1", never)
	}
	if age < 0 {
		t.Errorf("age = %s, want a non-negative span", age)
	}
}

// TestMarkObjectScrubbed_UnknownCopyIsNoOp verifies stamping a row that is not
// there fails silently rather than erroring, so a copy deleted mid-cycle does
// not fail the sweep.
func TestMarkObjectScrubbed_UnknownCopyIsNoOp(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.MarkObjectScrubbed(context.Background(), "bucket/missing", "backend-a"); err != nil {
		t.Errorf("stamping an absent copy should be a no-op, got %v", err)
	}
}

// TestScrubQueries_SurfaceDatabaseErrors verifies the scrub-queue reads and
// writes report a failing database rather than reporting an empty or zeroed
// result, which would read as "nothing to verify" and stall the sweep silently.
func TestScrubQueries_SurfaceDatabaseErrors(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.db.Close(); err != nil {
		t.Fatalf("closing the test database: %v", err)
	}

	if _, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, []string{"backend-a"}); err == nil {
		t.Error("GetLeastRecentlyScrubbedObjects should surface a closed database")
	}
	// A zero count from a failing database would read as "nothing deferred",
	// hiding exactly the backlog this figure exists to report.
	// A zero from a failing database would read as a fully encrypted fleet.
	if _, err := s.CountUnencryptedLocations(ctx); err == nil {
		t.Error("CountUnencryptedLocations should surface a closed database")
	}
	if _, err := s.CountScrubCandidatesOnBackends(ctx, []string{"backend-a"}); err == nil {
		t.Error("CountScrubCandidatesOnBackends should surface a closed database")
	}
	if err := s.MarkObjectScrubbed(ctx, "bucket/a", "backend-a"); err == nil {
		t.Error("MarkObjectScrubbed should surface a closed database")
	}
	if _, _, err := s.OldestUnverifiedAge(ctx); err == nil {
		t.Error("OldestUnverifiedAge should surface a closed database")
	}
}

// TestSqlite_ScrubQueue_BackendFilterExcludesUnaffordableBackends proves the
// filter is applied in SQL rather than by the caller. A copy on an excluded
// backend must not appear in the batch at all: if it did, the scrubber would
// have to either stamp it without reading it or leave it at the head of the
// queue forever.
func TestSqlite_ScrubQueue_BackendFilterExcludesUnaffordableBackends(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	for backend, key := range map[string]string{"backend-a": "bucket/a", "backend-b": "bucket/b"} {
		if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: backend, Size: 10}); err != nil {
			t.Fatalf("RecordObject(%s): %v", key, err)
		}
		if err := s.UpdateContentHash(ctx, key, backend, "sha256:"+key); err != nil {
			t.Fatalf("UpdateContentHash(%s): %v", key, err)
		}
	}

	got, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, []string{"backend-a"})
	if err != nil {
		t.Fatalf("GetLeastRecentlyScrubbedObjects: %v", err)
	}
	if len(got) != 1 || got[0].BackendName != "backend-a" {
		t.Fatalf("got %d copies %+v, want only the one on backend-a", len(got), got)
	}

	// An empty affordable set means nothing may be read, not everything.
	none, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, nil)
	if err != nil {
		t.Fatalf("GetLeastRecentlyScrubbedObjects(nil): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("an empty backend list returned %d copies, want 0", len(none))
	}
}

// TestSqlite_CountScrubCandidatesOnBackends counts the queue behind the
// backends a cycle declined, which is what lets a scrub report how much it left
// undone rather than only what it happened to sample.
func TestSqlite_CountScrubCandidatesOnBackends(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	for _, key := range []string{"bucket/x", "bucket/y"} {
		if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-b", Size: 10}); err != nil {
			t.Fatalf("RecordObject(%s): %v", key, err)
		}
		if err := s.UpdateContentHash(ctx, key, "backend-b", "sha256:"+key); err != nil {
			t.Fatalf("UpdateContentHash(%s): %v", key, err)
		}
	}
	// Hashless copies are not scrub candidates and must not be counted.
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/z", Backend: "backend-b", Size: 10}); err != nil {
		t.Fatalf("RecordObject(z): %v", err)
	}

	n, err := s.CountScrubCandidatesOnBackends(ctx, []string{"backend-b"})
	if err != nil {
		t.Fatalf("CountScrubCandidatesOnBackends: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (the hashed copies only)", n)
	}

	if n, err := s.CountScrubCandidatesOnBackends(ctx, nil); err != nil || n != 0 {
		t.Errorf("empty backend list: count=%d err=%v, want 0/nil", n, err)
	}
}

// -------------------------------------------------------------------------
// TIMESTAMP ORDERING
// -------------------------------------------------------------------------

// TestTimestampFormat_TextOrderMatchesChronologicalOrder is the property the
// canonical format exists for. These columns are TEXT, so ORDER BY compares
// them lexicographically; if the two orders can disagree, every queue and
// cutoff built on them is unreliable.
//
// The instants below are the shape that broke RFC3339Nano: one is a prefix of
// the other once trailing zeros are stripped, and 'Z' sorts above '0'.
func TestTimestampFormat_TextOrderMatchesChronologicalOrder(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 8, 11, 6, 0, 0, 0, time.UTC)
	instants := []time.Time{
		base,
		base.Add(1),                      // 1ns  - forces a nine-digit fraction
		base.Add(10 * time.Nanosecond),   // trailing zero
		base.Add(500 * time.Millisecond), // ".5" under RFC3339Nano
		base.Add(500*time.Millisecond + 10*time.Nanosecond),
		base.Add(time.Second),
	}

	for i := 1; i < len(instants); i++ {
		earlier, later := instants[i-1], instants[i]
		a, b := formatTime(earlier), formatTime(later)
		if a >= b {
			t.Errorf("text order disagrees with time order:\n  earlier %q\n  later   %q", a, b)
		}
		if len(a) != canonicalTimestampLen || len(b) != canonicalTimestampLen {
			t.Errorf("timestamps are not fixed width: %q (%d), %q (%d)", a, len(a), b, len(b))
		}
	}
}

// TestMigration0006_NormalizesLegacyTimestamps proves the migration repairs
// rows already on disk. Writing new rows correctly is not enough on its own: a
// padded value and an unpadded one are not mutually orderable, so a fleet
// upgraded mid-life would keep mis-ordering until the old rows were brought up.
func TestMigration0006_NormalizesLegacyTimestamps(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	// Write rows in the pre-canonical shape, exactly as RFC3339Nano rendered
	// them: variable fraction, and one with no fraction at all.
	legacy := map[string]string{
		"bucket/none":  "2026-08-11T06:00:00Z",
		"bucket/short": "2026-08-11T06:00:00.5Z",
		"bucket/long":  "2026-08-11T06:00:00.50000001Z",
	}
	for key, ts := range legacy {
		mustRecordObject(t, s, key, "backend-a", 100)
		if _, err := s.db.ExecContext(ctx,
			`UPDATE object_locations SET created_at = ?, last_scrubbed_at = NULL
			 WHERE object_key = ?`, ts, key); err != nil {
			t.Fatalf("seed legacy timestamp for %s: %v", key, err)
		}
	}

	// Rewind past 0006 and re-run so it applies to the seeded rows. Pinned to
	// the version before that migration rather than to whatever is newest, so
	// this keeps testing 0006 as later migrations land.
	rewindToSchemaVersion(t, s, 5)
	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT object_key, created_at FROM object_locations
		 WHERE object_key LIKE 'bucket/%' ORDER BY created_at`)
	if err != nil {
		t.Fatalf("read normalized rows: %v", err)
	}
	defer rows.Close()

	var order []string
	for rows.Next() {
		var key, ts string
		if err := rows.Scan(&key, &ts); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(ts) != canonicalTimestampLen {
			t.Errorf("%s kept a non-canonical timestamp %q (%d chars)", key, ts, len(ts))
		}
		order = append(order, key)
	}

	// Chronologically: none < short < long. Before the migration the text
	// comparison put "…00.5Z" after "…00.50000001Z".
	want := []string{"bucket/none", "bucket/short", "bucket/long"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("ORDER BY created_at gave %v, want %v", order, want)
		}
	}
}

// -------------------------------------------------------------------------
// MIGRATION LOADING
// -------------------------------------------------------------------------

// TestLoadMigrations_RejectsMalformedNames pins the naming rules the runner
// depends on. The version comes from the file name, so a name it cannot parse
// has to stop startup: guessing an order, or silently skipping the file, would
// leave a database that reports a version it never reached.
func TestLoadMigrations_RejectsMalformedNames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		file    string
		wantErr string
	}{
		{"no separator", "0006.sql", "is not named"},
		{"non-numeric version", "abc_thing.sql", "non-numeric version prefix"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := fstest.MapFS{
				migrationDir + "/" + tc.file: &fstest.MapFile{Data: []byte("SELECT 1;")},
			}
			_, err := loadMigrationsFrom(fsys)
			if err == nil {
				t.Fatalf("expected an error for %q, got nil", tc.file)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoadMigrations_OrdersByVersionAndIgnoresNonSQL checks the two things the
// runner assumes: ascending order regardless of directory listing order, and
// that unrelated files in the directory are skipped rather than parsed.
func TestLoadMigrations_OrdersByVersionAndIgnoresNonSQL(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		migrationDir + "/0010_ten.sql":   &fstest.MapFile{Data: []byte("SELECT 10;")},
		migrationDir + "/0002_two.sql":   &fstest.MapFile{Data: []byte("SELECT 2;")},
		migrationDir + "/0001_one.sql":   &fstest.MapFile{Data: []byte("SELECT 1;")},
		migrationDir + "/README.md":      &fstest.MapFile{Data: []byte("not a migration")},
		migrationDir + "/notes.sql.orig": &fstest.MapFile{Data: []byte("not a migration")},
	}

	got, err := loadMigrationsFrom(fsys)
	if err != nil {
		t.Fatalf("loadMigrationsFrom: %v", err)
	}
	want := []int{1, 2, 10}
	if len(got) != len(want) {
		t.Fatalf("loaded %d migrations, want %d: %+v", len(got), len(want), got)
	}
	for i, v := range want {
		if got[i].version != v {
			t.Errorf("position %d = version %d, want %d", i, got[i].version, v)
		}
	}
	// 10 must sort after 2, which string ordering on the file name would not do.
	if got[2].name != "ten" {
		t.Errorf("last migration name = %q, want %q", got[2].name, "ten")
	}
}

// TestLoadMigrations_MissingDirectoryIsAnError covers the read failure. An
// empty or absent migration set is not something to shrug at: it means the
// binary cannot bring a database to the version it claims to expect.
func TestLoadMigrations_MissingDirectoryIsAnError(t *testing.T) {
	t.Parallel()

	if _, err := loadMigrationsFrom(fstest.MapFS{}); err == nil {
		t.Fatal("expected an error when the migrations directory is absent")
	}
}

// TestRunMigrations_SurfacesDatabaseErrors keeps a failing database from
// reading as a successful migration. Returning nil here would let the process
// start against a database that was never brought up to date.
func TestRunMigrations_SurfacesDatabaseErrors(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.db.Close(); err != nil {
		t.Fatalf("closing the test database: %v", err)
	}
	if err := s.RunMigrations(ctx); err == nil {
		t.Error("RunMigrations should surface a closed database")
	}
}

// TestApplyMigration_FailedStatementRecordsNothing is the transactional
// guarantee. A migration whose SQL fails must leave no version row behind, or
// the next start would skip it and treat a database as migrated when it is not.
func TestApplyMigration_FailedStatementRecordsNothing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	bad := migration{version: expectedSchemaVersion + 1, name: "broken", sql: "THIS IS NOT SQL;"}
	if err := s.applyMigration(ctx, bad); err == nil {
		t.Fatal("expected an error from a migration with invalid SQL")
	}

	var rows int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_version WHERE version = ?`, bad.version).Scan(&rows); err != nil {
		t.Fatalf("count version rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("a failed migration recorded %d version rows, want 0", rows)
	}
}

// TestCountUnencryptedLocations counts what encrypt-existing would process, so
// the figure an operator sees matches the work the command would actually do.
func TestCountUnencryptedLocations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if n, err := s.CountUnencryptedLocations(ctx); err != nil || n != 0 {
		t.Fatalf("empty store: count=%d err=%v, want 0/nil", n, err)
	}

	mustRecordObject(t, s, "bucket/plain-a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/plain-b", "backend-a", 100)
	mustRecordObject(t, s, "bucket/secret", "backend-a", 100)
	if err := s.MarkObjectEncrypted(ctx, "bucket/secret", "backend-a",
		[]byte("wrapped"), "key-0", 100, 132); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}

	n, err := s.CountUnencryptedLocations(ctx)
	if err != nil {
		t.Fatalf("CountUnencryptedLocations: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (the encrypted copy must not be counted)", n)
	}

	// The count and the list agree, so the dashboard figure and the work
	// encrypt-existing performs cannot drift apart.
	listed, err := s.ListUnencryptedLocations(ctx, 100, core.Cursor{})
	if err != nil {
		t.Fatalf("ListUnencryptedLocations: %v", err)
	}
	if int64(len(listed)) != n {
		t.Errorf("count = %d but list returned %d rows", n, len(listed))
	}
}

// TestGetAllObjectLocations_ReportsVerifiedTimestamp pins the distinction the
// field exists for. A copy that has never been verified must stay
// distinguishable from one verified long ago: having a content hash only says a
// hash was recorded, not that the bytes were ever compared to it.
func TestGetAllObjectLocations_ReportsVerifiedTimestamp(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	const key = "bucket/replicated"
	mustRecordObject(t, s, key, "backend-a", 100)
	if _, _, err := s.RecordReplica(ctx, key, "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}
	for _, backend := range []string{"backend-a", "backend-b"} {
		if err := s.UpdateContentHash(ctx, key, backend, "sha256:x"); err != nil {
			t.Fatalf("UpdateContentHash(%s): %v", backend, err)
		}
	}

	// Verify one copy, leaving the other untouched. Per-copy is the point: a
	// replicated object can have one copy checked and another never looked at.
	if err := s.MarkObjectScrubbed(ctx, key, "backend-a"); err != nil {
		t.Fatalf("MarkObjectScrubbed: %v", err)
	}

	locs, err := s.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("expected 2 copies, got %d", len(locs))
	}

	byBackend := map[string]core.ObjectLocation{}
	for _, l := range locs {
		byBackend[l.BackendName] = l
	}

	verified := byBackend["backend-a"]
	if verified.LastScrubbedAt == nil {
		t.Error("the verified copy reports no timestamp")
	} else if verified.LastScrubbedAt.IsZero() {
		t.Error("the verified copy reports a zero timestamp")
	}

	if never := byBackend["backend-b"]; never.LastScrubbedAt != nil {
		t.Errorf("the never-verified copy reports %v, want nil", never.LastScrubbedAt)
	}
	// Both carry a hash, so the hash alone cannot tell the two apart.
	if byBackend["backend-a"].ContentHash == "" || byBackend["backend-b"].ContentHash == "" {
		t.Error("both copies should carry a content hash for this distinction to matter")
	}
}

// quotaBytesUsed reads a backend's current bytes_used counter.
func quotaBytesUsed(t *testing.T, s *Store, backend string) int64 {
	t.Helper()
	var used int64
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT bytes_used FROM backend_quotas WHERE backend_name = ?`, backend,
	).Scan(&used); err != nil {
		t.Fatalf("read bytes_used: %v", err)
	}
	return used
}

// TestMarkObjectDecrypted_DoesNotDriveQuotaNegative asserts a shrink larger
// than the counter holds clamps at zero instead of underflowing. bytes_used is
// per backend while the delta is per object, so an object whose recorded size
// outruns the counter - a stale size, or a second pass over an already
// decrypted copy - would otherwise leave a negative counter that over-admits
// every later write until ReconcileUsage runs.
func TestMarkObjectDecrypted_DoesNotDriveQuotaNegative(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/secret", "backend-a", 100)
	if err := s.MarkObjectEncrypted(ctx, "bucket/secret", "backend-a",
		[]byte("dek"), "key-1", 100, 900); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}

	// Drop the counter below what decryption is about to subtract, standing in
	// for a ledger that drifted from the recorded object sizes.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE backend_quotas SET bytes_used = 50 WHERE backend_name = ?`, "backend-a",
	); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// 900 -> 100 is a -800 delta against a counter holding 50.
	if err := s.MarkObjectDecrypted(ctx, "bucket/secret", "backend-a", 100); err != nil {
		t.Fatalf("MarkObjectDecrypted: %v", err)
	}
	if used := quotaBytesUsed(t, s, "backend-a"); used != 0 {
		t.Errorf("bytes_used = %d, want 0 - a negative counter over-admits later writes", used)
	}
}

// TestMarkObjectEncrypted_DoesNotDriveQuotaNegative asserts the encrypt path
// carries the same clamp. Encryption usually grows a copy, but the delta is
// signed and nothing in the schema forbids a ciphertext smaller than its
// plaintext.
func TestMarkObjectEncrypted_DoesNotDriveQuotaNegative(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/plain", "backend-a", 900)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE backend_quotas SET bytes_used = 50 WHERE backend_name = ?`, "backend-a",
	); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// 900 plaintext -> 100 ciphertext is a -800 delta against a counter of 50.
	if err := s.MarkObjectEncrypted(ctx, "bucket/plain", "backend-a",
		[]byte("dek"), "key-1", 900, 100); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}
	if used := quotaBytesUsed(t, s, "backend-a"); used != 0 {
		t.Errorf("bytes_used = %d, want 0", used)
	}
}

// TestMarkObjectEncrypted_ChargesTheCiphertextGrowth pins the ordinary case,
// so the clamp above cannot be mistaken for permission to lose real bytes.
func TestMarkObjectEncrypted_ChargesTheCiphertextGrowth(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/plain", "backend-a", 1000)
	before := quotaBytesUsed(t, s, "backend-a")

	if err := s.MarkObjectEncrypted(ctx, "bucket/plain", "backend-a",
		[]byte("dek"), "key-1", 1000, 1100); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}
	if got := quotaBytesUsed(t, s, "backend-a") - before; got != 100 {
		t.Errorf("bytes_used moved by %d, want the 100 bytes encryption added", got)
	}
}

// collationSensitiveKeys sort differently under byte order than under a locale
// collation: mixed case, punctuation and digits all sit on the boundary a
// locale ordering reshuffles. Mirrors the set the Postgres collation tests use,
// so both engines are pinned to one contract.
var collationSensitiveKeys = []string{
	"A", "a", "B", "b", "Zoo", "zoo",
	"a/b", "a-b", "a.b", "a_b",
	"A1", "a1", "ab", "Ab",
}

// TestListObjects_ByteOrder asserts the flat listing answers in UTF-8 byte
// order, which is what S3 ListObjectsV2 specifies. SQLite's BINARY collation
// gives this for free; the test exists so the contract is stated on both
// engines rather than only on the one that had to be corrected for it.
func TestListObjects_ByteOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	for _, k := range collationSensitiveKeys {
		mustRecordObject(t, s, k, "backend-a", 1)
	}

	var got []string
	cursor := ""
	for {
		res, err := s.ListObjects(ctx, "", cursor, 3)
		if err != nil {
			t.Fatalf("ListObjects(startAfter=%q): %v", cursor, err)
		}
		if len(res.Objects) == 0 {
			break
		}
		for i := range res.Objects {
			got = append(got, res.Objects[i].ObjectKey)
		}
		cursor = res.Objects[len(res.Objects)-1].ObjectKey
		if !res.IsTruncated {
			break
		}
	}

	want := slices.Clone(collationSensitiveKeys)
	slices.Sort(want) // Go string sort is byte order.
	if !slices.Equal(got, want) {
		t.Errorf("listing order mismatch:\n got  %q\n want %q", got, want)
	}
}

// TestListObjects_PaginationCoversEveryKeyOnce asserts paging over a
// collation-sensitive key set neither skips nor repeats.
func TestListObjects_PaginationCoversEveryKeyOnce(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	for _, k := range collationSensitiveKeys {
		mustRecordObject(t, s, k, "backend-a", 1)
	}

	seen := map[string]int{}
	cursor := ""
	for {
		res, err := s.ListObjects(ctx, "", cursor, 2)
		if err != nil {
			t.Fatalf("ListObjects(startAfter=%q): %v", cursor, err)
		}
		if len(res.Objects) == 0 {
			break
		}
		for i := range res.Objects {
			seen[res.Objects[i].ObjectKey]++
		}
		cursor = res.Objects[len(res.Objects)-1].ObjectKey
		if !res.IsTruncated {
			break
		}
	}

	for _, k := range collationSensitiveKeys {
		switch seen[k] {
		case 1: // covered exactly once, as intended
		case 0:
			t.Errorf("key %q was skipped across pages", k)
		default:
			t.Errorf("key %q was returned %d times across pages", k, seen[k])
		}
	}
}

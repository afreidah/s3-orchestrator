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
	"strings"
	"testing"
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
	})
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

// mustRecordObject records an object, failing the test on error.
func mustRecordObject(t *testing.T, s *Store, key, backend string, size int64) {
	t.Helper()
	if _, err := s.RecordObject(context.Background(), key, backend, size, nil); err != nil {
		t.Fatalf("RecordObject(%s, %s): %v", key, backend, err)
	}
}

// mustCreateUpload creates a multipart upload, failing the test on error.
func mustCreateUpload(t *testing.T, s *Store, uploadID, key, backend string) {
	t.Helper()
	if err := s.CreateMultipartUpload(context.Background(), uploadID, key, backend, "", nil); err != nil {
		t.Fatalf("CreateMultipartUpload(%s): %v", uploadID, err)
	}
}

// mustRecordReplica records a replica, failing the test on error. The
// size parameter is unused after #652 — the SQL now reads size from the
// source row inside the conditional INSERT — but the helper signature
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

// TestRecordObject_And_GetAllLocations verifies basic object recording and retrieval.
func TestRecordObject_And_GetAllLocations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	displaced, err := s.RecordObject(ctx, "bucket/key1", "backend-a", 1024, nil)
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
	displaced, err := s.RecordObject(ctx, "bucket/key1", "backend-b", 2048, nil)
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

// TestDeleteObject verifies that deleting an object removes it and returns the deleted copies.
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

// TestListObjectsByBackend verifies filtering objects by backend name.
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
// filter — successive pages skip rows already returned.
func TestListObjectsByBackendKeyAsc_HonoursCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	for _, k := range []string{"vb/a", "vb/b", "vb/c", "vb/d"} {
		mustRecordObject(t, s, k, "backend-a", 1)
	}

	// First page: cursor "" → ["vb/a", "vb/b"]
	page1, err := s.ListObjectsByBackendKeyAsc(ctx, "backend-a", "", 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 || page1[0].ObjectKey != "vb/a" || page1[1].ObjectKey != "vb/b" {
		t.Fatalf("page1 unexpected: %+v", page1)
	}

	// Second page: cursor "vb/b" → ["vb/c", "vb/d"]
	page2, err := s.ListObjectsByBackendKeyAsc(ctx, "backend-a", page1[1].ObjectKey, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 || page2[0].ObjectKey != "vb/c" || page2[1].ObjectKey != "vb/d" {
		t.Fatalf("page2 unexpected: %+v", page2)
	}

	// Third page: cursor "vb/d" → empty
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

	imported, err := s.ImportObject(ctx, "bucket/new", "backend-a", 500)
	if err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	if !imported {
		t.Error("expected imported=true for new object")
	}

	// Import again should be a no-op
	imported, err = s.ImportObject(ctx, "bucket/new", "backend-a", 500)
	if err != nil {
		t.Fatalf("ImportObject duplicate: %v", err)
	}
	if imported {
		t.Error("expected imported=false for duplicate")
	}
}

// TestMoveObjectLocation verifies atomic move of an object between backends.
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
// when the destination already holds a copy — MoveObjectLocation returns
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
// source row has already been removed — MoveObjectLocation returns
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
// backend — no DeletedCopy should be returned because the PutObject will
// overwrite in place.
func TestRecordObject_Overwrite_SameBackend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/k", "backend-a", 500)

	displaced, err := s.RecordObject(ctx, "bucket/k", "backend-a", 700, nil)
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if len(displaced) != 0 {
		t.Errorf("expected 0 displaced (same backend), got %d: %+v", len(displaced), displaced)
	}
}

// TestMoveObjectLocation_QuotaExceeded covers the ErrNoSpaceAvailable
// branch in moveObjectRows — the destination quota update touches zero
// rows when the move would exceed bytes_limit.
func TestMoveObjectLocation_QuotaExceeded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := NewStore(ctx, &config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{
		{Name: "big", QuotaBytes: 10_000},
		{Name: "small", QuotaBytes: 100}, // tiny — cannot hold 500-byte object
	}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	if _, err := s.RecordObject(ctx, "bucket/huge", "big", 500, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	_, err = s.MoveObjectLocation(ctx, "bucket/huge", "big", "small")
	if err != core.ErrNoSpaceAvailable {
		t.Errorf("expected ErrNoSpaceAvailable, got %v", err)
	}
}

// TestRecordObject_QuotaExceeded covers the ErrNoSpaceAvailable branch
// in incrementSQLiteQuota — the guarded UPDATE touches zero rows when
// the quota ceiling would be exceeded.
func TestRecordObject_QuotaExceeded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := NewStore(ctx, &config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{
		{Name: "tight", QuotaBytes: 100},
	}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	_, err = s.RecordObject(ctx, "bucket/over", "tight", 500, nil)
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

// TestRecordObject_WithEncryption verifies storing and retrieving encryption metadata.
func TestRecordObject_WithEncryption(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	enc := &core.EncryptionMeta{
		Encrypted:     true,
		EncryptionKey: []byte("wrapped-dek"),
		KeyID:         "key-1",
		PlaintextSize: 1024,
		ContentHash:   "abc123",
	}
	_, err := s.RecordObject(ctx, "bucket/encrypted", "backend-a", 1100, enc)
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

// TestGetBackendWithSpace verifies pack routing selects a backend with available quota.
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

// TestGetLeastUtilizedBackend verifies spread routing selects the least utilized backend.
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

// TestOrphanBytes verifies orphan byte tracking and adjustment.
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
	err := s.CreateMultipartUpload(ctx, "upload-1", "bucket/photo.png", "backend-a", "image/png", meta)
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

// TestCountActiveMultipartUploads verifies counting active uploads by bucket prefix.
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

// TestRecordReplica_Duplicate verifies that recording a duplicate replica is idempotent.
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

// TestRemoveExcessCopy verifies removal of an excess replica and quota adjustment.
func TestRemoveExcessCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/key1", "backend-a", 1024)
	mustRecordReplica(t, s, "bucket/key1", "backend-b", "backend-a", 1024)

	if err := s.RemoveExcessCopy(ctx, "bucket/key1", "backend-b", 1024); err != nil {
		t.Fatalf("RemoveExcessCopy: %v", err)
	}

	locs, _ := s.GetAllObjectLocations(ctx, "bucket/key1")
	if len(locs) != 1 {
		t.Errorf("expected 1 copy after removal, got %d", len(locs))
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

// TestCleanupQueue_Retry verifies exponential backoff retry for failed cleanup items.
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

// TestIntegrity_HashOperations verifies content hash storage and retrieval for integrity checks.
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
	hashed, err := s.GetRandomHashedObjects(ctx, 10)
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

// TestListDirectoryChildren verifies virtual directory listing with common prefixes.
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

// TestListExpiredObjects verifies listing objects older than a cutoff time.
func TestListExpiredObjects(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/old", "backend-a", 100)
	mustRecordObject(t, s, "bucket/new", "backend-a", 200)

	// Everything is "new" (just created) — none should be expired
	cutoff := time.Now().Add(-time.Hour)
	expired, err := s.ListExpiredObjects(ctx, "bucket/", cutoff, 10)
	if err != nil {
		t.Fatalf("ListExpiredObjects: %v", err)
	}
	if len(expired) != 0 {
		t.Errorf("expected 0 expired, got %d", len(expired))
	}

	// Use a future cutoff — everything should be expired
	expired, _ = s.ListExpiredObjects(ctx, "bucket/", time.Now().Add(time.Hour), 10)
	if len(expired) != 2 {
		t.Errorf("expected 2 expired with future cutoff, got %d", len(expired))
	}
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
	unenc, err := s.ListUnencryptedLocations(ctx, 10, 0)
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

// TestNotificationOutbox_Retry verifies retry semantics for failed notification deliveries.
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

// TestWithAdvisoryLock verifies basic advisory lock acquisition and callback execution.
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

// TestDeleteBackendData verifies that all objects for a backend are removed.
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

// TestVerifySchemaVersion verifies that a matching schema version passes validation.
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

	_, err = s.ListExpiredObjects(ctx, "bucket/", time.Now().Add(time.Hour), 100)
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

// -------------------------------------------------------------------------
// ADDITIONAL COVERAGE
// -------------------------------------------------------------------------

// TestDeleteObjectLocation verifies removal of a single object replica by backend.
func TestDeleteObjectLocation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/key1", "backend-a", 100)
	mustRecordReplica(t, s, "bucket/key1", "backend-b", "backend-a", 100)

	if err := s.DeleteObjectLocation(ctx, "bucket/key1", "backend-b"); err != nil {
		t.Fatalf("DeleteObjectLocation: %v", err)
	}

	locs, _ := s.GetAllObjectLocations(ctx, "bucket/key1")
	if len(locs) != 1 || locs[0].BackendName != "backend-a" {
		t.Errorf("expected only backend-a, got %+v", locs)
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

// TestGetStaleMultipartUploads verifies detection of uploads older than a threshold.
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

// TestGetMultipartUploadsByBackend verifies listing uploads filtered by backend.
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

// TestCountOverReplicatedObjects verifies counting objects with more copies than the replication factor.
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

// TestListAllEncryptedLocations verifies paginated listing of encrypted object locations.
func TestListAllEncryptedLocations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	enc := &core.EncryptionMeta{
		Encrypted:     true,
		EncryptionKey: []byte("dek"),
		KeyID:         "key-1",
		PlaintextSize: 1024,
	}
	if _, err := s.RecordObject(ctx, "bucket/enc", "backend-a", 1100, enc); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	locs, err := s.ListAllEncryptedLocations(ctx, 10, 0)
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

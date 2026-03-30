// -------------------------------------------------------------------------------
// SQLite Store Tests - Full MetadataStore and AdminStore Contract Coverage
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
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store"
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

// mustRecordReplica records a replica, failing the test on error.
func mustRecordReplica(t *testing.T, s *Store, key, target, source string, size int64) {
	t.Helper()
	if _, err := s.RecordReplica(context.Background(), key, target, source, size); err != nil {
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

func TestRecordObject_And_GetAllLocations(t *testing.T) {
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

func TestRecordObject_Overwrite_DisplacesCopy(t *testing.T) {
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

func TestDeleteObject(t *testing.T) {
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
	if err != store.ErrObjectNotFound {
		t.Errorf("expected ErrObjectNotFound, got %v", err)
	}
}

func TestDeleteObject_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.DeleteObject(ctx, "bucket/nonexistent")
	if err != store.ErrObjectNotFound {
		t.Errorf("expected ErrObjectNotFound, got %v", err)
	}
}

func TestListObjects(t *testing.T) {
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

func TestListObjects_Pagination(t *testing.T) {
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

func TestListObjectsByBackend(t *testing.T) {
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

func TestImportObject(t *testing.T) {
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

func TestMoveObjectLocation(t *testing.T) {
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

func TestBackendObjectStats(t *testing.T) {
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

func TestRecordObject_WithEncryption(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	enc := &store.EncryptionMeta{
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

func TestGetBackendWithSpace(t *testing.T) {
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

func TestGetLeastUtilizedBackend(t *testing.T) {
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

func TestGetQuotaStats(t *testing.T) {
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

func TestOrphanBytes(t *testing.T) {
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

func TestFlushUsageDeltas_And_GetUsage(t *testing.T) {
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

func TestMultipartUpload_Lifecycle(t *testing.T) {
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
	if err != store.ErrMultipartUploadNotFound {
		t.Errorf("expected ErrMultipartUploadNotFound, got %v", err)
	}
}

func TestListMultipartUploads(t *testing.T) {
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

func TestCountActiveMultipartUploads(t *testing.T) {
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

func TestGetActiveMultipartCounts(t *testing.T) {
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

func TestReplication_UnderAndOver(t *testing.T) {
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
	inserted, err := s.RecordReplica(ctx, "bucket/key1", "backend-b", "backend-a", 1024)
	if err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}
	if !inserted {
		t.Error("expected replica to be inserted")
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

func TestRecordReplica_Duplicate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/key1", "backend-a", 1024)
	mustRecordReplica(t, s, "bucket/key1", "backend-b", "backend-a", 1024)

	// Duplicate replica should return false
	inserted, err := s.RecordReplica(ctx, "bucket/key1", "backend-b", "backend-a", 1024)
	if err != nil {
		t.Fatalf("RecordReplica duplicate: %v", err)
	}
	if inserted {
		t.Error("expected inserted=false for duplicate replica")
	}
}

func TestRemoveExcessCopy(t *testing.T) {
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

func TestCleanupQueue_Lifecycle(t *testing.T) {
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

func TestCleanupQueue_Retry(t *testing.T) {
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

// -------------------------------------------------------------------------
// INTEGRITY
// -------------------------------------------------------------------------

func TestIntegrity_HashOperations(t *testing.T) {
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

func TestListDirectoryChildren(t *testing.T) {
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

// -------------------------------------------------------------------------
// LIFECYCLE (EXPIRATION)
// -------------------------------------------------------------------------

func TestListExpiredObjects(t *testing.T) {
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

func TestEncryptionAdmin_MarkAndList(t *testing.T) {
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

func TestNotificationOutbox_Lifecycle(t *testing.T) {
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

func TestNotificationOutbox_Retry(t *testing.T) {
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

func TestWithAdvisoryLock(t *testing.T) {
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

// -------------------------------------------------------------------------
// BACKEND LIFECYCLE
// -------------------------------------------------------------------------

func TestDeleteBackendData(t *testing.T) {
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

func TestVerifySchemaVersion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.VerifySchemaVersion(ctx); err != nil {
		t.Errorf("VerifySchemaVersion: %v", err)
	}
}

// -------------------------------------------------------------------------
// ADDITIONAL COVERAGE
// -------------------------------------------------------------------------

func TestDeleteObjectLocation(t *testing.T) {
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

func TestGetObjectCounts(t *testing.T) {
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

func TestGetStaleMultipartUploads(t *testing.T) {
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

func TestGetMultipartUploadsByBackend(t *testing.T) {
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

func TestGetUnderReplicatedObjectsExcluding(t *testing.T) {
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

func TestCountOverReplicatedObjects(t *testing.T) {
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

func TestListAllEncryptedLocations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	enc := &store.EncryptionMeta{
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

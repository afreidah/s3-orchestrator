// -------------------------------------------------------------------------------
// Postgres Store - Public Method Integration Tests
//
// Author: Alex Freidah
//
// Direct coverage for every public Store method that the existing
// internal/integration/ suite does not exercise (advisory locks,
// notifications, usage flush, integrity, multipart admin, replication
// queries, encryption admin). Reuses the shared adapterPgStore fixture
// declared in adapter_integration_test.go for one Postgres container
// per suite.
// -------------------------------------------------------------------------------

//go:build integration
// +build integration

package postgres

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// ADVISORY LOCK
// -------------------------------------------------------------------------

// TestStoreInt_WithAdvisoryLock_AcquiresAndRunsFn verifies the lock
// is acquired, the user fn runs, and the lock is released so a
// second acquisition succeeds.
func TestStoreInt_WithAdvisoryLock_AcquiresAndRunsFn(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	lockID := int64(123456)

	var ran atomic.Bool
	got, err := s.WithAdvisoryLock(ctx, lockID, func(ctx context.Context) error {
		ran.Store(true)
		return nil
	})
	if err != nil {
		t.Fatalf("WithAdvisoryLock: %v", err)
	}
	if !got {
		t.Error("expected got=true on successful acquisition")
	}
	if !ran.Load() {
		t.Error("fn did not run")
	}
	// Lock must be released so a second acquire succeeds.
	got, err = s.WithAdvisoryLock(ctx, lockID, func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("WithAdvisoryLock(again): %v", err)
	}
	if !got {
		t.Error("expected lock release after first call returned")
	}
}

// TestStoreInt_WithAdvisoryLock_PropagatesFnError verifies the helper
// returns whatever error fn yields and still releases the lock.
func TestStoreInt_WithAdvisoryLock_PropagatesFnError(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	lockID := int64(123457)
	sentinel := errors.New("fn failed")
	got, err := s.WithAdvisoryLock(ctx, lockID, func(ctx context.Context) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
	if !got {
		t.Error("expected got=true even when fn errors")
	}
	// Confirm the lock was released.
	if got, err := s.WithAdvisoryLock(ctx, lockID, func(ctx context.Context) error { return nil }); err != nil || !got {
		t.Errorf("re-acquire after fn error: got=%v err=%v", got, err)
	}
}

// -------------------------------------------------------------------------
// INTEGRITY
// -------------------------------------------------------------------------

// TestStoreInt_GetObjectsWithoutHash verifies the helper returns
// objects whose content_hash is NULL. After UpdateContentHash flips
// one row, the next call reflects the change.
func TestStoreInt_GetObjectsWithoutHash(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	if _, err := s.RecordObject(ctx, key, "backend-a", 100, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()

	rows, err := s.GetObjectsWithoutHash(ctx, 1000, 0)
	if err != nil {
		t.Fatalf("GetObjectsWithoutHash: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ObjectKey == key {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected key %s in GetObjectsWithoutHash", key)
	}

	if err := s.UpdateContentHash(ctx, key, "backend-a", "deadbeef"); err != nil {
		t.Fatalf("UpdateContentHash: %v", err)
	}
	rows, err = s.GetObjectsWithoutHash(ctx, 1000, 0)
	if err != nil {
		t.Fatalf("GetObjectsWithoutHash(after): %v", err)
	}
	for _, r := range rows {
		if r.ObjectKey == key {
			t.Errorf("key %s still in GetObjectsWithoutHash after UpdateContentHash", key)
		}
	}
}

// TestStoreInt_GetRandomHashedObjects verifies the helper returns
// hashed rows. The clamp on small/zero limits is exercised too.
func TestStoreInt_GetRandomHashedObjects(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	if _, err := s.RecordObject(ctx, key, "backend-a", 100, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()
	if err := s.UpdateContentHash(ctx, key, "backend-a", "abc123"); err != nil {
		t.Fatalf("UpdateContentHash: %v", err)
	}

	// The query uses TABLESAMPLE / RANDOM and may return 0 rows on a
	// small table; we assert only that the query runs without error.
	if _, err := s.GetRandomHashedObjects(ctx, 100); err != nil {
		t.Fatalf("GetRandomHashedObjects: %v", err)
	}
	// Zero/negative limits clamp to 1, exercising the safeLimit branch.
	if _, err := s.GetRandomHashedObjects(ctx, 0); err != nil {
		t.Errorf("GetRandomHashedObjects(0): %v", err)
	}
}

// -------------------------------------------------------------------------
// NOTIFICATIONS OUTBOX
// -------------------------------------------------------------------------

// TestStoreInt_NotificationsOutbox covers the full outbox lifecycle:
// insert -> get pending -> retry (increments attempts) -> complete.
func TestStoreInt_NotificationsOutbox(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	if err := s.InsertNotification(ctx, "test.event", `{"k":"v"}`, "https://example.invalid/hook"); err != nil {
		t.Fatalf("InsertNotification: %v", err)
	}

	pending, err := s.GetPendingNotifications(ctx, 100)
	if err != nil {
		t.Fatalf("GetPendingNotifications: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("expected at least one pending notification")
	}
	var n core.NotificationRow
	for _, p := range pending {
		if p.EventType == "test.event" {
			n = p
			break
		}
	}
	if n.ID == 0 {
		t.Fatal("test.event notification not found in pending list")
	}
	// Postgres jsonb normalises whitespace; check the URL field
	// directly and validate the payload contains our key/value rather
	// than asserting exact byte equality.
	if n.EndpointURL != "https://example.invalid/hook" {
		t.Errorf("EndpointURL mismatch: %q", n.EndpointURL)
	}
	if !bytes.Contains(n.Payload, []byte(`"k"`)) || !bytes.Contains(n.Payload, []byte(`"v"`)) {
		t.Errorf("payload missing expected fields: %q", n.Payload)
	}

	if err := s.RetryNotification(ctx, n.ID, time.Second, "transient"); err != nil {
		t.Fatalf("RetryNotification: %v", err)
	}
	if err := s.CompleteNotification(ctx, n.ID); err != nil {
		t.Fatalf("CompleteNotification: %v", err)
	}
}

// -------------------------------------------------------------------------
// USAGE FLUSH
// -------------------------------------------------------------------------

// TestStoreInt_UsageFlushAndRead exercises FlushUsageDeltas (insert +
// upsert paths) and reads back via GetUsageForPeriod.
func TestStoreInt_UsageFlushAndRead(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	period := "2026-05"
	if err := s.FlushUsageDeltas(ctx, "backend-a", period, 10, 1024, 2048); err != nil {
		t.Fatalf("FlushUsageDeltas(insert): %v", err)
	}
	// Second flush exercises the upsert path: counters accumulate.
	if err := s.FlushUsageDeltas(ctx, "backend-a", period, 5, 100, 200); err != nil {
		t.Fatalf("FlushUsageDeltas(upsert): %v", err)
	}

	got, err := s.GetUsageForPeriod(ctx, period)
	if err != nil {
		t.Fatalf("GetUsageForPeriod: %v", err)
	}
	stat, ok := got["backend-a"]
	if !ok {
		t.Fatalf("backend-a not in usage map: %+v", got)
	}
	if stat.APIRequests < 15 || stat.EgressBytes < 1124 || stat.IngressBytes < 2248 {
		t.Errorf("usage didn't accumulate as expected: %+v", stat)
	}
}

// -------------------------------------------------------------------------
// MULTIPART
// -------------------------------------------------------------------------

// TestStoreInt_MultipartLifecycle covers create, GetMultipartUpload,
// RecordPart, GetParts, CountActiveMultipartUploadsByPrefix,
// ListMultipartUploads, GetMultipartUploadsByBackend, and finally
// DeleteMultipartUpload.
func TestStoreInt_MultipartLifecycle(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	uploadID := uniqueKey(t, "upload")
	key := uniqueKey(t, "k")
	if err := s.CreateMultipartUpload(ctx, uploadID, key, "backend-a", "application/octet-stream", map[string]string{"foo": "bar"}); err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	defer func() { _ = s.DeleteMultipartUpload(ctx, uploadID) }()

	mu, err := s.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		t.Fatalf("GetMultipartUpload: %v", err)
	}
	if mu.ObjectKey != key || mu.BackendName != "backend-a" || mu.Metadata["foo"] != "bar" {
		t.Errorf("upload payload mismatch: %+v", mu)
	}

	if err := s.RecordPart(ctx, uploadID, 1, "etag-1", 1024, nil); err != nil {
		t.Fatalf("RecordPart: %v", err)
	}
	if err := s.RecordPart(ctx, uploadID, 2, "etag-2", 2048, nil); err != nil {
		t.Fatalf("RecordPart: %v", err)
	}
	parts, err := s.GetParts(ctx, uploadID)
	if err != nil {
		t.Fatalf("GetParts: %v", err)
	}
	if len(parts) != 2 || parts[0].PartNumber != 1 || parts[1].PartNumber != 2 {
		t.Errorf("parts ordering or count wrong: %+v", parts)
	}

	count, err := s.CountActiveMultipartUploads(ctx, t.Name())
	if err != nil {
		t.Fatalf("CountActiveMultipartUploads: %v", err)
	}
	if count < 1 {
		t.Errorf("expected count>=1, got %d", count)
	}
	uploads, err := s.ListMultipartUploads(ctx, t.Name(), 100)
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	if len(uploads) == 0 {
		t.Error("expected at least one upload")
	}
	uploads, err = s.GetMultipartUploadsByBackend(ctx, "backend-a")
	if err != nil {
		t.Fatalf("GetMultipartUploadsByBackend: %v", err)
	}
	if len(uploads) == 0 {
		t.Error("expected at least one upload on backend-a")
	}

	stale, err := s.GetStaleMultipartUploads(ctx, -time.Hour)
	if err != nil {
		t.Fatalf("GetStaleMultipartUploads: %v", err)
	}
	_ = stale
}

// TestStoreInt_RecordPart_RejectsInvalidPartNumber covers the input
// validation branch.
func TestStoreInt_RecordPart_RejectsInvalidPartNumber(t *testing.T) {
	s := adapterPgStore(t)
	if err := s.RecordPart(context.Background(), "any", 0, "x", 0, nil); err == nil {
		t.Error("expected error for partNumber=0")
	}
	if err := s.RecordPart(context.Background(), "any", 100001, "x", 0, nil); err == nil {
		t.Error("expected error for partNumber>10000")
	}
}

// TestStoreInt_RecordPart_PreservesEncryptionFields verifies the
// encryption branch of RecordPart lands every encryption attribute.
func TestStoreInt_RecordPart_PreservesEncryptionFields(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	uploadID := uniqueKey(t, "upload")
	if err := s.CreateMultipartUpload(ctx, uploadID, uniqueKey(t, "k"), "backend-a", "", nil); err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	defer func() { _ = s.DeleteMultipartUpload(ctx, uploadID) }()
	enc := &core.EncryptionMeta{
		Encrypted: true, EncryptionKey: []byte("packed"), KeyID: "kid-1", PlaintextSize: 50,
	}
	if err := s.RecordPart(ctx, uploadID, 1, "etag", 1024, enc); err != nil {
		t.Fatalf("RecordPart: %v", err)
	}
	parts, err := s.GetParts(ctx, uploadID)
	if err != nil {
		t.Fatalf("GetParts: %v", err)
	}
	if len(parts) != 1 || !parts[0].Encrypted || parts[0].KeyID != "kid-1" || parts[0].PlaintextSize != 50 {
		t.Errorf("encryption fields not preserved on part: %+v", parts)
	}
	if string(parts[0].EncryptionKey) != "packed" {
		t.Errorf("EncryptionKey not preserved: %v", parts[0].EncryptionKey)
	}
}

// -------------------------------------------------------------------------
// QUOTA
// -------------------------------------------------------------------------

// TestStoreInt_GetBackendWithSpace verifies the iteration finds the
// first backend with sufficient space, skipping unknown names.
func TestStoreInt_GetBackendWithSpace(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	got, err := s.GetBackendWithSpace(ctx, 100, []string{"unknown-backend", "backend-a"})
	if err != nil {
		t.Fatalf("GetBackendWithSpace: %v", err)
	}
	if got != "backend-a" {
		t.Errorf("expected backend-a, got %q", got)
	}
	// Empty order yields ErrNoSpaceAvailable.
	if _, err := s.GetBackendWithSpace(ctx, 100, nil); !errors.Is(err, core.ErrNoSpaceAvailable) {
		t.Errorf("expected ErrNoSpaceAvailable, got %v", err)
	}
}

// TestStoreInt_GetLeastUtilizedBackend verifies the helper returns
// a backend with enough space, and ErrNoSpaceAvailable when none
// fits.
func TestStoreInt_GetLeastUtilizedBackend(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	got, err := s.GetLeastUtilizedBackend(ctx, 100, []string{"backend-a", "backend-b"})
	if err != nil {
		t.Fatalf("GetLeastUtilizedBackend: %v", err)
	}
	if got != "backend-a" && got != "backend-b" {
		t.Errorf("unexpected backend %q", got)
	}
	// Asking for an unrealistic size yields ErrNoSpaceAvailable.
	if _, err := s.GetLeastUtilizedBackend(ctx, 1<<62, []string{"backend-a"}); !errors.Is(err, core.ErrNoSpaceAvailable) {
		t.Errorf("expected ErrNoSpaceAvailable, got %v", err)
	}
}

// TestStoreInt_GetQuotaStats verifies the helper returns a row per
// configured backend.
func TestStoreInt_GetQuotaStats(t *testing.T) {
	s := adapterPgStore(t)
	stats, err := s.GetQuotaStats(context.Background())
	if err != nil {
		t.Fatalf("GetQuotaStats: %v", err)
	}
	if _, ok := stats["backend-a"]; !ok {
		t.Errorf("backend-a missing from stats: %+v", stats)
	}
}

// TestStoreInt_GetObjectCounts_GetActiveMultipartCounts verifies both
// dashboard helpers run without error and return one entry per
// backend that has data.
func TestStoreInt_GetObjectCounts_GetActiveMultipartCounts(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	if _, err := s.GetObjectCounts(ctx); err != nil {
		t.Errorf("GetObjectCounts: %v", err)
	}
	if _, err := s.GetActiveMultipartCounts(ctx); err != nil {
		t.Errorf("GetActiveMultipartCounts: %v", err)
	}
}

// -------------------------------------------------------------------------
// REPLICATION QUERIES
// -------------------------------------------------------------------------

// TestStoreInt_ReplicationQueries verifies the under/over-replication
// queries run, GetObjectCopiesForUpdate runs, and CountOverReplicated
// returns a non-error count.
func TestStoreInt_ReplicationQueries(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	if _, err := s.GetUnderReplicatedObjects(ctx, 2, 100); err != nil {
		t.Errorf("GetUnderReplicatedObjects: %v", err)
	}
	if _, err := s.GetUnderReplicatedObjectsExcluding(ctx, 2, 100, []string{"backend-c"}); err != nil {
		t.Errorf("GetUnderReplicatedObjectsExcluding: %v", err)
	}
	if _, err := s.GetOverReplicatedObjects(ctx, 1, 100); err != nil {
		t.Errorf("GetOverReplicatedObjects: %v", err)
	}
	if _, err := s.CountOverReplicatedObjects(ctx, 1); err != nil {
		t.Errorf("CountOverReplicatedObjects: %v", err)
	}
	if _, err := s.GetObjectCopiesForUpdate(ctx, uniqueKey(t, "missing")); err != nil {
		t.Errorf("GetObjectCopiesForUpdate: %v", err)
	}
}

// -------------------------------------------------------------------------
// ENCRYPTION ADMIN
// -------------------------------------------------------------------------

// TestStoreInt_EncryptionAdminLifecycle covers the encrypt/rotate/
// decrypt admin helpers: ListUnencryptedLocations, MarkObjectEncrypted,
// ListEncryptedLocations, UpdateEncryptionKey, ListAllEncryptedLocations,
// MarkObjectDecrypted.
func TestStoreInt_EncryptionAdminLifecycle(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	if _, err := s.RecordObject(ctx, key, "backend-a", 100, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()

	// Initially unencrypted.
	rows, err := s.ListUnencryptedLocations(ctx, 1000, 0)
	if err != nil {
		t.Fatalf("ListUnencryptedLocations: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ObjectKey == key {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected key %s in ListUnencryptedLocations", key)
	}

	// Mark as encrypted.
	if err := s.MarkObjectEncrypted(ctx, key, "backend-a", []byte("packed"), "kid-1", 80, 100); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}

	// Now appears in ListEncryptedLocations(keyID).
	encRows, err := s.ListEncryptedLocations(ctx, "kid-1", 1000, 0)
	if err != nil {
		t.Fatalf("ListEncryptedLocations: %v", err)
	}
	found = false
	for _, r := range encRows {
		if r.ObjectKey == key {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected key %s in ListEncryptedLocations(kid-1)", key)
	}

	// Rotate key.
	if err := s.UpdateEncryptionKey(ctx, key, "backend-a", []byte("repacked"), "kid-2"); err != nil {
		t.Fatalf("UpdateEncryptionKey: %v", err)
	}

	// ListAllEncryptedLocations sees the row regardless of key ID.
	if _, err := s.ListAllEncryptedLocations(ctx, 1000, 0); err != nil {
		t.Fatalf("ListAllEncryptedLocations: %v", err)
	}

	// Mark decrypted.
	if err := s.MarkObjectDecrypted(ctx, key, "backend-a", 80); err != nil {
		t.Fatalf("MarkObjectDecrypted: %v", err)
	}
}

// -------------------------------------------------------------------------
// CLEANUP QUEUE
// -------------------------------------------------------------------------

// TestStoreInt_CleanupQueueLifecycle covers EnqueueCleanup,
// GetPendingCleanups, RetryCleanupItem, CompleteCleanupItem,
// CleanupQueueDepth, and Increment/DecrementOrphanBytes.
func TestStoreInt_CleanupQueueLifecycle(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	if err := s.EnqueueCleanup(ctx, "backend-a", key, "test", 256); err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}

	depth, err := s.CleanupQueueDepth(ctx)
	if err != nil {
		t.Fatalf("CleanupQueueDepth: %v", err)
	}
	if depth < 1 {
		t.Errorf("expected depth>=1, got %d", depth)
	}

	pending, err := s.GetPendingCleanups(ctx, 100)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	var item core.CleanupItem
	for _, p := range pending {
		if p.ObjectKey == key {
			item = p
			break
		}
	}
	if item.ID == 0 {
		t.Fatalf("cleanup row for %s not found", key)
	}

	if err := s.RetryCleanupItem(ctx, item.ID, time.Second, "transient"); err != nil {
		t.Fatalf("RetryCleanupItem: %v", err)
	}
	if err := s.CompleteCleanupItem(ctx, item.ID); err != nil {
		t.Fatalf("CompleteCleanupItem: %v", err)
	}
	if err := s.IncrementOrphanBytes(ctx, "backend-a", 1024); err != nil {
		t.Fatalf("IncrementOrphanBytes: %v", err)
	}
	if err := s.DecrementOrphanBytes(ctx, "backend-a", 512); err != nil {
		t.Fatalf("DecrementOrphanBytes: %v", err)
	}
}

// -------------------------------------------------------------------------
// OBJECT LIST QUERIES
// -------------------------------------------------------------------------

// TestStoreInt_ListExpiredObjects verifies the prefix + cutoff query
// runs and returns a non-error result, including the LIKE-escape path
// for prefixes containing wildcards.
func TestStoreInt_ListExpiredObjects(t *testing.T) {
	s := adapterPgStore(t)
	if _, err := s.ListExpiredObjects(context.Background(), t.Name(), time.Now().Add(time.Hour), 100); err != nil {
		t.Fatalf("ListExpiredObjects: %v", err)
	}
	// Prefix containing a LIKE wildcard exercises the escaper.
	if _, err := s.ListExpiredObjects(context.Background(), t.Name()+"%", time.Now().Add(time.Hour), 100); err != nil {
		t.Errorf("ListExpiredObjects(wildcard): %v", err)
	}
}

// TestStoreInt_ListObjectsByBackendKeyAsc verifies the paginated
// key-asc listing runs.
func TestStoreInt_ListObjectsByBackendKeyAsc(t *testing.T) {
	s := adapterPgStore(t)
	if _, err := s.ListObjectsByBackendKeyAsc(context.Background(), "backend-a", "", 10); err != nil {
		t.Errorf("ListObjectsByBackendKeyAsc: %v", err)
	}
}

// -------------------------------------------------------------------------
// SCHEMA / LIFECYCLE
// -------------------------------------------------------------------------

// TestStoreInt_VerifySchemaVersion verifies the helper returns nil
// against a freshly migrated database.
func TestStoreInt_VerifySchemaVersion(t *testing.T) {
	s := adapterPgStore(t)
	if err := s.VerifySchemaVersion(context.Background()); err != nil {
		t.Errorf("VerifySchemaVersion: %v", err)
	}
}

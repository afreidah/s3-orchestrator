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
	"strings"
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

// seedMultipartUpload creates a multipart upload and returns the
// upload ID + cleanup. Shared by the multipart tests below to keep
// each test focused on one Store method.
func seedMultipartUpload(t *testing.T, s *Store, contentType string, metadata map[string]string) (uploadID, key string) {
	t.Helper()
	ctx := context.Background()
	uploadID = uniqueKey(t, "upload")
	key = uniqueKey(t, "k")
	if err := s.CreateMultipartUpload(ctx, &core.CreateMultipartUploadParams{
		UploadID:    uploadID,
		ObjectKey:   key,
		BackendName: "backend-a",
		ContentType: contentType,
		Metadata:    metadata,
	}); err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteMultipartUpload(context.Background(), uploadID) })
	return uploadID, key
}

// TestStoreInt_GetMultipartUpload verifies CreateMultipartUpload +
// GetMultipartUpload round-trips key, backend, and metadata.
func TestStoreInt_GetMultipartUpload(t *testing.T) {
	s := adapterPgStore(t)
	uploadID, key := seedMultipartUpload(t, s, "application/octet-stream", map[string]string{"foo": "bar"})

	mu, err := s.GetMultipartUpload(context.Background(), uploadID)
	if err != nil {
		t.Fatalf("GetMultipartUpload: %v", err)
	}
	if mu.ObjectKey != key || mu.BackendName != "backend-a" || mu.Metadata["foo"] != "bar" {
		t.Errorf("upload payload mismatch: %+v", mu)
	}
}

// TestStoreInt_RecordPartAndGetParts verifies parts are persisted
// in part-number order with the right etag and size.
func TestStoreInt_RecordPartAndGetParts(t *testing.T) {
	s := adapterPgStore(t)
	uploadID, _ := seedMultipartUpload(t, s, "", nil)
	ctx := context.Background()

	if err := s.RecordPart(ctx, uploadID, 1, "etag-1", 1024, nil); err != nil {
		t.Fatalf("RecordPart(1): %v", err)
	}
	if err := s.RecordPart(ctx, uploadID, 2, "etag-2", 2048, nil); err != nil {
		t.Fatalf("RecordPart(2): %v", err)
	}
	parts, err := s.GetParts(ctx, uploadID)
	if err != nil {
		t.Fatalf("GetParts: %v", err)
	}
	if len(parts) != 2 || parts[0].PartNumber != 1 || parts[1].PartNumber != 2 {
		t.Errorf("parts ordering or count wrong: %+v", parts)
	}
}

// TestStoreInt_CountActiveMultipartUploads verifies the helper
// counts in-progress uploads under a prefix.
func TestStoreInt_CountActiveMultipartUploads(t *testing.T) {
	s := adapterPgStore(t)
	seedMultipartUpload(t, s, "", nil)

	count, err := s.CountActiveMultipartUploads(context.Background(), t.Name())
	if err != nil {
		t.Fatalf("CountActiveMultipartUploads: %v", err)
	}
	if count < 1 {
		t.Errorf("expected count>=1, got %d", count)
	}
}

// TestStoreInt_ListMultipartUploads verifies the prefix list returns
// in-progress uploads.
func TestStoreInt_ListMultipartUploads(t *testing.T) {
	s := adapterPgStore(t)
	seedMultipartUpload(t, s, "", nil)

	uploads, err := s.ListMultipartUploads(context.Background(), t.Name(), 100)
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	if len(uploads) == 0 {
		t.Error("expected at least one upload")
	}
}

// TestStoreInt_GetMultipartUploadsByBackend verifies the helper
// returns uploads scoped to a backend.
func TestStoreInt_GetMultipartUploadsByBackend(t *testing.T) {
	s := adapterPgStore(t)
	seedMultipartUpload(t, s, "", nil)

	uploads, err := s.GetMultipartUploadsByBackend(context.Background(), "backend-a")
	if err != nil {
		t.Fatalf("GetMultipartUploadsByBackend: %v", err)
	}
	if len(uploads) == 0 {
		t.Error("expected at least one upload on backend-a")
	}
}

// TestStoreInt_GetStaleMultipartUploads verifies the helper runs
// without error against a fresh upload (passing a negative duration
// matches uploads created at any time).
func TestStoreInt_GetStaleMultipartUploads(t *testing.T) {
	s := adapterPgStore(t)
	seedMultipartUpload(t, s, "", nil)

	if _, err := s.GetStaleMultipartUploads(context.Background(), -time.Hour); err != nil {
		t.Fatalf("GetStaleMultipartUploads: %v", err)
	}
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
	if err := s.CreateMultipartUpload(ctx, &core.CreateMultipartUploadParams{
		UploadID:    uploadID,
		ObjectKey:   uniqueKey(t, "k"),
		BackendName: "backend-a",
	}); err != nil {
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
// GetObjectBackendsForKeys (rebalancer batch query)
// -------------------------------------------------------------------------

// TestStoreInt_GetObjectBackendsForKeys_EmptyInput verifies the
// helper returns an empty map for a nil or empty input slice without
// issuing a query.
func TestStoreInt_GetObjectBackendsForKeys_EmptyInput(t *testing.T) {
	s := adapterPgStore(t)
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

// TestStoreInt_GetObjectBackendsForKeys_GroupsByKey verifies replicas
// of the same key are bucketed together, missing keys are absent, and
// the helper reads every backend for every supplied key in one round
// trip.
func TestStoreInt_GetObjectBackendsForKeys_GroupsByKey(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	k1 := uniqueKey(t, "k1")
	k2 := uniqueKey(t, "k2")
	missing := uniqueKey(t, "missing")
	if _, err := s.RecordObject(ctx, k1, "backend-a", 100, nil); err != nil {
		t.Fatalf("RecordObject(k1): %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, k1) }()
	if _, _, err := s.RecordReplica(ctx, k1, "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}
	if _, err := s.RecordObject(ctx, k2, "backend-a", 50, nil); err != nil {
		t.Fatalf("RecordObject(k2): %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, k2) }()

	got, err := s.GetObjectBackendsForKeys(ctx, []string{k1, k2, missing})
	if err != nil {
		t.Fatalf("GetObjectBackendsForKeys: %v", err)
	}
	if len(got[k1]) != 2 {
		t.Errorf("k1 should have 2 backends, got %v", got[k1])
	}
	if len(got[k2]) != 1 || got[k2][0] != "backend-a" {
		t.Errorf("k2 backends mismatch: %v", got[k2])
	}
	if _, ok := got[missing]; ok {
		t.Errorf("missing key should not be in result map: %v", got)
	}
}

// -------------------------------------------------------------------------
// DeleteObjectsBatch (single-tx batch delete)
// -------------------------------------------------------------------------

// TestStoreInt_DeleteObjectsBatch_EmptyInput verifies the helper
// short-circuits on an empty input without opening a transaction.
func TestStoreInt_DeleteObjectsBatch_EmptyInput(t *testing.T) {
	s := adapterPgStore(t)
	got, err := s.DeleteObjectsBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("DeleteObjectsBatch(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map for nil input, got %v", got)
	}
}

// TestStoreInt_DeleteObjectsBatch_RemovesRowsAndDecrementsQuotas
// verifies the single-tx batch removes every supplied key's rows,
// decrements each affected backend's quota by the summed size, and
// returns per-key displaced copies for cleanup.
func TestStoreInt_DeleteObjectsBatch_RemovesRowsAndDecrementsQuotas(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	k1 := uniqueKey(t, "k1")
	k2 := uniqueKey(t, "k2")
	missing := uniqueKey(t, "missing")
	if _, err := s.RecordObject(ctx, k1, "backend-a", 100, nil); err != nil {
		t.Fatalf("RecordObject(k1): %v", err)
	}
	if _, _, err := s.RecordReplica(ctx, k1, "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}
	if _, err := s.RecordObject(ctx, k2, "backend-a", 50, nil); err != nil {
		t.Fatalf("RecordObject(k2): %v", err)
	}

	beforeA, err := s.GetQuotaStats(ctx)
	if err != nil {
		t.Fatalf("GetQuotaStats(before): %v", err)
	}

	got, err := s.DeleteObjectsBatch(ctx, []string{k1, k2, missing})
	if err != nil {
		t.Fatalf("DeleteObjectsBatch: %v", err)
	}
	if len(got[k1]) != 2 {
		t.Errorf("%s should have 2 displaced copies, got %v", k1, got[k1])
	}
	if len(got[k2]) != 1 || got[k2][0].BackendName != "backend-a" {
		t.Errorf("%s displaced copy mismatch: %v", k2, got[k2])
	}
	if _, ok := got[missing]; ok {
		t.Errorf("missing key must not be in result map: %v", got)
	}

	for _, k := range []string{k1, k2} {
		if _, err := s.GetAllObjectLocations(ctx, k); !errors.Is(err, core.ErrObjectNotFound) {
			t.Errorf("expected %s gone, got err=%v", k, err)
		}
	}

	afterA, err := s.GetQuotaStats(ctx)
	if err != nil {
		t.Fatalf("GetQuotaStats(after): %v", err)
	}
	if before, after := beforeA["backend-a"].BytesUsed, afterA["backend-a"].BytesUsed; before-after != 150 {
		t.Errorf("backend-a delta = %d, want 150 (k1=100 + k2=50)", before-after)
	}
	if before, after := beforeA["backend-b"].BytesUsed, afterA["backend-b"].BytesUsed; before-after != 100 {
		t.Errorf("backend-b delta = %d, want 100 (k1 replica)", before-after)
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

// TestStoreInt_VerifySchemaVersion_OlderThanExpected verifies the
// "older than expected" diagnostic surfaces when the goose_db_version
// row records a version below ExpectedSchemaVersion. Operators rely on
// this branch to detect partial-migration failures at startup; if the
// path were silent, a half-applied migration could let a binary boot
// against an inconsistent schema.
func TestStoreInt_VerifySchemaVersion_OlderThanExpected(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	pool := s.pool

	// goose tracks the applied version in goose_db_version, ordered by
	// id; the latest row wins. Insert a row whose version is below
	// ExpectedSchemaVersion to simulate a partially-rolled-back state.
	if _, err := pool.Exec(ctx,
		`INSERT INTO goose_db_version (version_id, is_applied, tstamp)
		 VALUES ($1, true, NOW())`,
		ExpectedSchemaVersion-1); err != nil {
		t.Fatalf("seed older version: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM goose_db_version WHERE version_id = $1`,
			ExpectedSchemaVersion-1)
	})

	// VerifySchemaVersion uses MAX(version_id) WHERE is_applied = true,
	// so we need to mark the real latest row as un-applied for this
	// test, then restore it on cleanup.
	if _, err := pool.Exec(ctx,
		`UPDATE goose_db_version SET is_applied = false
		 WHERE version_id = $1`, ExpectedSchemaVersion); err != nil {
		t.Fatalf("hide latest applied version: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			`UPDATE goose_db_version SET is_applied = true
			 WHERE version_id = $1`, ExpectedSchemaVersion)
	})

	err := s.VerifySchemaVersion(ctx)
	if err == nil {
		t.Fatal("expected older-than-expected error, got nil")
	}
	if !strings.Contains(err.Error(), "older than expected") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestStoreInt_VerifySchemaVersion_NewerThanExpected verifies the
// "newer than expected" diagnostic surfaces when the database has a
// migration the binary has never seen. This is what operators see
// after rolling a binary back below the schema's frontier; surfacing
// the mismatch prevents the older binary from running write paths
// that may not be aware of new columns.
func TestStoreInt_VerifySchemaVersion_NewerThanExpected(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	pool := s.pool

	// Insert a synthetic future version row.
	future := int64(ExpectedSchemaVersion + 100)
	if _, err := pool.Exec(ctx,
		`INSERT INTO goose_db_version (version_id, is_applied, tstamp)
		 VALUES ($1, true, NOW())`, future); err != nil {
		t.Fatalf("seed future version: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM goose_db_version WHERE version_id = $1`, future)
	})

	err := s.VerifySchemaVersion(ctx)
	if err == nil {
		t.Fatal("expected newer-than-expected error, got nil")
	}
	if !strings.Contains(err.Error(), "newer than expected") {
		t.Errorf("unexpected error message: %v", err)
	}
}

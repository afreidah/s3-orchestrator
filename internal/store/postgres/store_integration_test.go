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

package postgres

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
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

// TestStoreInt_WithAdvisoryLock_ConcurrentExclusivity pins the most
// load-bearing invariant in the worker fleet: two acquirers of the
// same lockID never run their fn concurrently. Without this the
// leader-election story collapses and every background worker
// (replicator, rebalancer, cleanup, drain, etc.) could run twice in
// parallel across instances.
func TestStoreInt_WithAdvisoryLock_ConcurrentExclusivity(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	lockID := int64(123458)

	// A holds the lock for 200ms. B races to acquire while A is in fn.
	// pg_try_advisory_lock returns false on contention (no wait), so B
	// must see (false, nil).
	started := make(chan struct{})
	release := make(chan struct{})
	var aRan, bRan atomic.Bool

	var wg sync.WaitGroup
	wg.Go(func() {
		got, err := s.WithAdvisoryLock(ctx, lockID, func(ctx context.Context) error {
			aRan.Store(true)
			close(started)
			<-release
			return nil
		})
		if err != nil || !got {
			t.Errorf("A: got=%v err=%v, want true,nil", got, err)
		}
	})

	<-started
	gotB, errB := s.WithAdvisoryLock(ctx, lockID, func(ctx context.Context) error {
		bRan.Store(true)
		return nil
	})
	if errB != nil {
		t.Errorf("B err = %v, want nil", errB)
	}
	if gotB {
		t.Error("B acquired the lock while A held it")
	}
	if bRan.Load() {
		t.Error("B's fn ran despite A holding the lock")
	}

	close(release)
	wg.Wait()

	if !aRan.Load() {
		t.Error("A's fn did not run")
	}

	// Lock released; a fresh acquire must succeed.
	got, err := s.WithAdvisoryLock(ctx, lockID, func(ctx context.Context) error { return nil })
	if err != nil || !got {
		t.Errorf("post-release acquire: got=%v err=%v, want true,nil", got, err)
	}
}

// TestStoreInt_WithAdvisoryLock_ReleasedOnPanic confirms the deferred
// pg_advisory_unlock fires even when fn panics, so a worker bug cannot
// permanently strand a lockID.
func TestStoreInt_WithAdvisoryLock_ReleasedOnPanic(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	lockID := int64(123459)

	func() {
		defer func() { _ = recover() }()
		_, _ = s.WithAdvisoryLock(ctx, lockID, func(ctx context.Context) error {
			panic("simulated fn panic")
		})
	}()

	got, err := s.WithAdvisoryLock(ctx, lockID, func(ctx context.Context) error { return nil })
	if err != nil || !got {
		t.Errorf("re-acquire after fn panic: got=%v err=%v, want true,nil (lock should have been released)", got, err)
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
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()

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

// TestStoreInt_GetLeastRecentlyScrubbedObjects verifies the helper returns
// hashed rows. The clamp on small/zero limits is exercised too.
func TestStoreInt_GetLeastRecentlyScrubbedObjects(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()
	if err := s.UpdateContentHash(ctx, key, "backend-a", "abc123"); err != nil {
		t.Fatalf("UpdateContentHash: %v", err)
	}

	if _, err := s.GetLeastRecentlyScrubbedObjects(ctx, 100, []string{"backend-a"}); err != nil {
		t.Fatalf("GetLeastRecentlyScrubbedObjects: %v", err)
	}
	// Zero/negative limits clamp to 1, exercising the safeLimit branch.
	if _, err := s.GetLeastRecentlyScrubbedObjects(ctx, 0, []string{"backend-a"}); err != nil {
		t.Errorf("GetLeastRecentlyScrubbedObjects(0): %v", err)
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

// TestStoreInt_PoolUsageFlushAndRead exercises the per-pool counters
// admission is judged against: the insert and additive-upsert paths of
// FlushPoolDeltas, and the period-scoped read that seeds the baselines.
func TestStoreInt_PoolUsageFlushAndRead(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	period := "2026-06"

	if err := s.FlushPoolDeltas(ctx, "backend-a", period, core.PoolUsage{"class_a": 10, "class_b": 4}); err != nil {
		t.Fatalf("FlushPoolDeltas(insert): %v", err)
	}
	// Second flush exercises the upsert path, which is what lets several
	// instances flush the same period without losing each other's deltas.
	if err := s.FlushPoolDeltas(ctx, "backend-a", period, core.PoolUsage{"class_a": 5}); err != nil {
		t.Fatalf("FlushPoolDeltas(upsert): %v", err)
	}
	// A zero delta writes no row: a pool nothing charged should not report as
	// active from the first tick of a period.
	if err := s.FlushPoolDeltas(ctx, "backend-a", period, core.PoolUsage{"class_c": 0}); err != nil {
		t.Fatalf("FlushPoolDeltas(zero): %v", err)
	}

	got, err := s.GetPoolUsageForPeriod(ctx, period)
	if err != nil {
		t.Fatalf("GetPoolUsageForPeriod: %v", err)
	}
	pools, ok := got["backend-a"]
	if !ok {
		t.Fatalf("backend-a not in pool usage map: %+v", got)
	}
	if pools["class_a"] < 15 {
		t.Errorf("class_a = %d, want at least 15 after the upsert", pools["class_a"])
	}
	if pools["class_b"] < 4 {
		t.Errorf("class_b = %d, want at least 4", pools["class_b"])
	}
	if _, charged := pools["class_c"]; charged {
		t.Errorf("class_c has a row: %+v; a zero delta must write nothing", pools)
	}
}

// TestStoreInt_PoolUsageIsScopedToPeriod pins the monthly rollover: budgets
// reset because the read is keyed by period, with no reset job to run.
func TestStoreInt_PoolUsageIsScopedToPeriod(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	if err := s.FlushPoolDeltas(ctx, "backend-a", "2026-07", core.PoolUsage{"class_a": 9}); err != nil {
		t.Fatalf("FlushPoolDeltas: %v", err)
	}

	got, err := s.GetPoolUsageForPeriod(ctx, "2026-08")
	if err != nil {
		t.Fatalf("GetPoolUsageForPeriod: %v", err)
	}
	if pools, ok := got["backend-a"]; ok {
		t.Errorf("backend-a carried %v into the next period, want none", pools)
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

	if err := s.RecordPart(ctx, &core.RecordPartParams{UploadID: uploadID, PartNumber: 1, ETag: "etag-1", SizeBytes: 1024, Form: nil}); err != nil {
		t.Fatalf("RecordPart(1): %v", err)
	}
	if err := s.RecordPart(ctx, &core.RecordPartParams{UploadID: uploadID, PartNumber: 2, ETag: "etag-2", SizeBytes: 2048, Form: nil}); err != nil {
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
	if err := s.RecordPart(context.Background(), &core.RecordPartParams{UploadID: "any", PartNumber: 0, ETag: "x"}); err == nil {
		t.Error("expected error for partNumber=0")
	}
	if err := s.RecordPart(context.Background(), &core.RecordPartParams{UploadID: "any", PartNumber: 100001, ETag: "x"}); err == nil {
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
	form := &core.StoredForm{
		Encrypted: true, EncryptionKey: []byte("packed"), KeyID: "kid-1", PlaintextSize: 50,
	}
	if err := s.RecordPart(ctx, &core.RecordPartParams{UploadID: uploadID, PartNumber: 1, ETag: "etag", SizeBytes: 1024, Form: form}); err != nil {
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

// TestStoreInt_ListBackendQuotaUsage verifies the routing view carries a row
// per configured backend with the three byte totals a placement decision is
// judged against. This is the query that replaced the placement SQL: the
// tracker loads it once per flush and decides in memory from there, so a row
// missing a total would route against a backend that is fuller than it reports.
func TestStoreInt_ListBackendQuotaUsage(t *testing.T) {
	s := adapterPgStore(t)
	usage, err := s.ListBackendQuotaUsage(context.Background())
	if err != nil {
		t.Fatalf("ListBackendQuotaUsage: %v", err)
	}
	byName := make(map[string]core.BackendQuotaUsage, len(usage))
	for _, u := range usage {
		byName[u.BackendName] = u
	}
	got, ok := byName["backend-a"]
	if !ok {
		t.Fatalf("backend-a missing from usage: %+v", usage)
	}
	if got.BytesUsed < 0 || got.OrphanBytes < 0 || got.InflightBytes < 0 {
		t.Errorf("negative byte total in usage row: %+v", got)
	}
	if got.Occupied() != got.BytesUsed+got.OrphanBytes+got.InflightBytes {
		t.Errorf("Occupied disagrees with its parts: %+v", got)
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

// TestStoreInt_GetUnverifiedObjectCounts pins the dashboard #405 helper:
// runs the per-backend NULL-content_hash query against the real
// Postgres + sqlc-generated query.
func TestStoreInt_GetUnverifiedObjectCounts(t *testing.T) {
	s := adapterPgStore(t)
	if _, err := s.GetUnverifiedObjectCounts(context.Background()); err != nil {
		t.Errorf("GetUnverifiedObjectCounts: %v", err)
	}
}

// -------------------------------------------------------------------------
// REPLICATION QUERIES
// -------------------------------------------------------------------------

// TestStoreInt_ReplicationQueries verifies the under/over-replication
// queries run and CountOverReplicated returns a non-error count.
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
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()

	// Initially unencrypted.
	rows, err := s.ListUnencryptedLocations(ctx, 1000, core.Cursor{})
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
	if err := s.MarkObjectEncrypted(ctx, &core.EncryptedUpdate{
		ObjectKey: key, BackendName: "backend-a", EncryptionKey: []byte("packed"),
		KeyID: "kid-1", PlaintextSize: 80, CiphertextSize: 100,
	}); err != nil {
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
	if _, err := s.ListAllEncryptedLocations(ctx, 1000, core.Cursor{}); err != nil {
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
	if _, err := s.ListExpiredObjects(context.Background(), core.ExpiredObjectsQuery{
		Prefix: t.Name(), Cutoff: time.Now().Add(time.Hour), Limit: 100,
	}); err != nil {
		t.Fatalf("ListExpiredObjects: %v", err)
	}
	// Prefix containing a LIKE wildcard exercises the escaper.
	if _, err := s.ListExpiredObjects(context.Background(), core.ExpiredObjectsQuery{
		Prefix: t.Name() + "%", Cutoff: time.Now().Add(time.Hour), Limit: 100,
	}); err != nil {
		t.Errorf("ListExpiredObjects(wildcard): %v", err)
	}
}

// TestStoreInt_ImportSuppressedByPendingCleanup proves the suppression against
// real Postgres, where the check is a UNION over cleanup_queue and cleanup_dlq
// rather than the SQLite variant the unit tests cover.
//
// Without it a delete that could not reach a backend is undone the next time
// reconcile walks it: the object returns live, the replicator spreads it, and
// its created_at restarts so a lifecycle rule that expired it waits another
// full window.
func TestStoreInt_ImportSuppressedByPendingCleanup(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := t.Name() + "/deleted"

	if err := s.EnqueueCleanup(ctx, "backend-a", key, "delete_failed", 500); err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}

	outcome, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: key, Backend: "backend-a", Size: 500})
	if err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	if outcome != core.ImportSkippedPendingCleanup {
		t.Errorf("outcome = %s, want skipped_pending_cleanup", outcome)
	}

	if _, err := s.GetAllObjectLocations(ctx, key); !errors.Is(err, core.ErrObjectNotFound) {
		t.Errorf("ledger error = %v, want ErrObjectNotFound for a suppressed import", err)
	}

	// Scoped to the backend the delete is outstanding on: a copy removed
	// cleanly elsewhere must still be importable, or one stuck cleanup would
	// block the whole key from ever being reconciled.
	other, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: key, Backend: "backend-b", Size: 500})
	if err != nil {
		t.Fatalf("ImportObject(backend-b): %v", err)
	}
	if other != core.ImportInserted {
		t.Errorf("outcome = %s, want inserted on a backend with no pending delete", other)
	}
}

// expiredKeysPg runs one query against real Postgres and returns the keys it
// selected, sorted, so a test can compare against a literal.
func expiredKeysPg(t *testing.T, s *Store, q core.ExpiredObjectsQuery) []string {
	t.Helper()
	rows, err := s.ListExpiredObjects(context.Background(), q)
	if err != nil {
		t.Fatalf("ListExpiredObjects: %v", err)
	}
	keys := make([]string, 0, len(rows))
	for i := range rows {
		keys = append(keys, rows[i].ObjectKey)
	}
	sort.Strings(keys)
	return keys
}

// TestStoreInt_ListExpiredObjectsTagFilter proves the lifecycle tag filter
// against real Postgres. The unit tests cover the SQLite variant only, so this
// is the only thing exercising the jsonb_each_text join, the tag_count
// equality that makes several tags an intersection, and the interaction with
// the DISTINCT ON that reduces an object's replicas to one row.
func TestStoreInt_ListExpiredObjectsTagFilter(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	prefix := t.Name() + "/"

	objects := []struct {
		key  string
		tags []core.Tag
	}{
		{prefix + "both", []core.Tag{{Key: "env", Value: "staging"}, {Key: "team", Value: "infra"}}},
		{prefix + "one", []core.Tag{{Key: "env", Value: "staging"}}},
		{prefix + "other", []core.Tag{{Key: "env", Value: "prod"}, {Key: "team", Value: "infra"}}},
		{prefix + "none", nil},
	}
	for _, o := range objects {
		if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
			Key: o.key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100, Tags: o.tags,
		}); err != nil {
			t.Fatalf("RecordObject %s: %v", o.key, err)
		}
	}

	// A second copy of one key, so a tag-filtered query has to dedup replicas
	// rather than trivially returning one row per object.
	if _, _, err := s.RecordReplica(ctx, prefix+"both", "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}

	future := time.Now().Add(time.Hour)
	base := core.ExpiredObjectsQuery{Prefix: prefix, Cutoff: future, Limit: 100}

	cases := []struct {
		name string
		tags map[string]string
		want []string
	}{
		{
			name: "no filter selects every object once",
			want: []string{prefix + "both", prefix + "none", prefix + "one", prefix + "other"},
		},
		{
			name: "one tag",
			tags: map[string]string{"env": "staging"},
			want: []string{prefix + "both", prefix + "one"},
		},
		{
			name: "two tags select the intersection",
			tags: map[string]string{"env": "staging", "team": "infra"},
			want: []string{prefix + "both"},
		},
		{
			name: "a tag the objects carry with a different value matches nothing",
			tags: map[string]string{"env": "nonexistent"},
			want: []string{},
		},
		{
			name: "keys and values are not matched independently",
			tags: map[string]string{"env": "infra", "team": "staging"},
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := base
			q.Tags = tc.tags
			if got := expiredKeysPg(t, s, q); !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("cutoff still applies alongside the tags", func(t *testing.T) {
		q := base
		q.Tags = map[string]string{"env": "staging"}
		q.Cutoff = time.Now().Add(-time.Hour)
		if got := expiredKeysPg(t, s, q); len(got) != 0 {
			t.Errorf("got %v, want nothing for a past cutoff", got)
		}
	})
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
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: k1, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100}); err != nil {
		t.Fatalf("RecordObject(k1): %v", err)
	}
	defer func() { _, _, _ = s.DeleteObject(ctx, k1) }()
	if _, _, err := s.RecordReplica(ctx, k1, "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: k2, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 50}); err != nil {
		t.Fatalf("RecordObject(k2): %v", err)
	}
	defer func() { _, _, _ = s.DeleteObject(ctx, k2) }()

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
	got, _, err := s.DeleteObjectsBatch(context.Background(), nil)
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
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: k1, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100}); err != nil {
		t.Fatalf("RecordObject(k1): %v", err)
	}
	if _, _, err := s.RecordReplica(ctx, k1, "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: k2, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 50}); err != nil {
		t.Fatalf("RecordObject(k2): %v", err)
	}

	beforeA, err := s.GetQuotaStats(ctx)
	if err != nil {
		t.Fatalf("GetQuotaStats(before): %v", err)
	}

	got, _, err := s.DeleteObjectsBatch(ctx, []string{k1, k2, missing})
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

// TestStoreInt_ScrubQueue_FreshWritesDoNotJumpTheQueue pins the property that
// keeps the sweep alive on a busy fleet. Ordering purely on the verified
// timestamp put every new write at the head of the queue, so once writes
// outpaced the scrubber nothing older was ever reached.
func TestStoreInt_ScrubQueue_FreshWritesDoNotJumpTheQueue(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	oldKey := uniqueKey(t, "old")
	freshKey := uniqueKey(t, "fresh")
	for _, key := range []string{oldKey, freshKey} {
		if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100}); err != nil {
			t.Fatalf("RecordObject(%s): %v", key, err)
		}
		defer func() { _, _, _ = s.DeleteObject(ctx, key) }()
		if err := s.UpdateContentHash(ctx, key, "backend-a", "abc123"); err != nil {
			t.Fatalf("UpdateContentHash(%s): %v", key, err)
		}
	}

	// oldKey was written a year ago and verified a month ago; freshKey was
	// written just now and has never been verified.
	if _, err := s.pool.Exec(ctx,
		`UPDATE object_locations
		 SET created_at = NOW() - interval '365 days',
		     last_scrubbed_at = NOW() - interval '30 days'
		 WHERE object_key = $1`, oldKey); err != nil {
		t.Fatalf("backdating %s: %v", oldKey, err)
	}

	got, err := s.GetLeastRecentlyScrubbedObjects(ctx, 100, []string{"backend-a"})
	if err != nil {
		t.Fatalf("GetLeastRecentlyScrubbedObjects: %v", err)
	}

	var oldPos, freshPos = -1, -1
	for i := range got {
		switch got[i].ObjectKey {
		case oldKey:
			oldPos = i
		case freshKey:
			freshPos = i
		}
	}
	if oldPos == -1 || freshPos == -1 {
		t.Fatalf("expected both copies in the queue, got old=%d fresh=%d", oldPos, freshPos)
	}
	if oldPos > freshPos {
		t.Errorf("a copy verified a month ago sorted behind one written moments ago (old=%d fresh=%d)",
			oldPos, freshPos)
	}
}

// TestStoreInt_ScrubQueue_IndexMatchesQuery verifies the partial expression
// index can serve the candidate query, which is what keeps the sweep from
// sorting the whole ledger once it is large.
//
// Sequential scans are disabled rather than asserting the planner picks the
// index unprompted: on a test-sized table a sequential scan is genuinely
// cheaper and choosing it is correct. What matters here is that the index is
// usable at all, since an ORDER BY expression that drifts from the indexed one
// would still fall back to a sort with sequential scans off.
func TestStoreInt_ScrubQueue_IndexMatchesQuery(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring a connection: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("disabling seqscan: %v", err)
	}

	rows, err := conn.Query(ctx, `EXPLAIN (COSTS OFF)
		SELECT object_key FROM object_locations
		WHERE content_hash IS NOT NULL AND managed
		ORDER BY COALESCE(last_scrubbed_at, created_at) ASC, object_key ASC
		LIMIT 100`)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scanning plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if !strings.Contains(plan.String(), "idx_object_locations_scrub_queue") {
		t.Errorf("the scrub queue index cannot serve the candidate query, so the "+
			"ORDER BY and the index expression have drifted apart:\n%s", plan.String())
	}
}

// TestStoreInt_ScrubQueue_BackendFilter proves the Postgres selection applies
// the affordable-backend filter in SQL. Filtering after selection would force
// the scrubber to either stamp a copy it never read or leave it at the head of
// the queue to be re-selected every cycle.
func TestStoreInt_ScrubQueue_BackendFilter(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	keyA := uniqueKey(t, "affordable")
	keyB := uniqueKey(t, "declined")
	for backend, key := range map[string]string{"backend-a": keyA, "backend-b": keyB} {
		if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: backend}}, Size: 10}); err != nil {
			t.Fatalf("RecordObject(%s): %v", key, err)
		}
		defer func() { _, _, _ = s.DeleteObject(ctx, key) }()
		if err := s.UpdateContentHash(ctx, key, backend, "abc123"); err != nil {
			t.Fatalf("UpdateContentHash(%s): %v", key, err)
		}
	}

	got, err := s.GetLeastRecentlyScrubbedObjects(ctx, 100, []string{"backend-a"})
	if err != nil {
		t.Fatalf("GetLeastRecentlyScrubbedObjects: %v", err)
	}
	for _, loc := range got {
		if loc.BackendName != "backend-a" {
			t.Fatalf("batch contains a copy on %s, which was not offered", loc.BackendName)
		}
	}

	// An empty affordable set selects nothing rather than everything.
	none, err := s.GetLeastRecentlyScrubbedObjects(ctx, 100, nil)
	if err != nil {
		t.Fatalf("GetLeastRecentlyScrubbedObjects(nil): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("an empty backend list returned %d copies, want 0", len(none))
	}

	n, err := s.CountScrubCandidatesOnBackends(ctx, []string{"backend-b"})
	if err != nil {
		t.Fatalf("CountScrubCandidatesOnBackends: %v", err)
	}
	if n < 1 {
		t.Errorf("count on the declined backend = %d, want at least the one copy written here", n)
	}
	if n, err := s.CountScrubCandidatesOnBackends(ctx, nil); err != nil || n != 0 {
		t.Errorf("empty backend list: count=%d err=%v, want 0/nil", n, err)
	}
}

// TestStoreInt_CountUnencryptedLocations pins the figure the dashboard, the
// status endpoint and the plaintext gauge all read. It has to agree with
// ListUnencryptedLocations, because that is the set encrypt-existing processes:
// a count that drifts from the work would tell an operator the fleet is covered
// when it is not.
func TestStoreInt_CountUnencryptedLocations(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	before, err := s.CountUnencryptedLocations(ctx)
	if err != nil {
		t.Fatalf("CountUnencryptedLocations: %v", err)
	}

	key := uniqueKey(t, "plaintext")
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()

	after, err := s.CountUnencryptedLocations(ctx)
	if err != nil {
		t.Fatalf("CountUnencryptedLocations: %v", err)
	}
	if after != before+1 {
		t.Errorf("count = %d after writing one plaintext copy, want %d", after, before+1)
	}

	// Encrypting the copy removes it from the count, so the figure falls as an
	// operator works through the backlog rather than staying put.
	if err := s.MarkObjectEncrypted(ctx, &core.EncryptedUpdate{
		ObjectKey: key, BackendName: "backend-a", EncryptionKey: []byte("wrapped"),
		KeyID: "key-0", PlaintextSize: 100, CiphertextSize: 132,
	}); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}
	encrypted, err := s.CountUnencryptedLocations(ctx)
	if err != nil {
		t.Fatalf("CountUnencryptedLocations: %v", err)
	}
	if encrypted != before {
		t.Errorf("count = %d after encrypting the copy, want %d", encrypted, before)
	}

	// The count and the list describe the same set.
	listed, err := s.ListUnencryptedLocations(ctx, 10000, core.Cursor{})
	if err != nil {
		t.Fatalf("ListUnencryptedLocations: %v", err)
	}
	if int64(len(listed)) != encrypted {
		t.Errorf("count = %d but list returned %d rows", encrypted, len(listed))
	}
}

// TestStoreInt_GetAllObjectLocations_ReportsVerifiedTimestamp pins the field
// per copy against a real database. Having a content hash only says a hash was
// recorded; this says whether the bytes were ever compared to it, and the two
// copies here differ on exactly that while sharing everything else.
func TestStoreInt_GetAllObjectLocations_ReportsVerifiedTimestamp(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	key := uniqueKey(t, "verified")
	// Hashed at write, not by backfill, which stamps: the replica insert carries
	// the hash across without the stamp, leaving two hashed copies that differ
	// only on whether the bytes were ever read back.
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100,
		Form: &core.StoredForm{ContentHash: "abc123"},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()
	if _, _, err := s.RecordReplica(ctx, key, "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}

	if err := s.MarkObjectScrubbed(ctx, key, "backend-a"); err != nil {
		t.Fatalf("MarkObjectScrubbed: %v", err)
	}

	locs, err := s.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	byBackend := map[string]core.ObjectLocation{}
	for _, l := range locs {
		byBackend[l.BackendName] = l
	}

	verified, ok := byBackend["backend-a"]
	if !ok {
		t.Fatalf("backend-a copy missing from %v", locs)
	}
	if verified.LastScrubbedAt == nil || verified.LastScrubbedAt.IsZero() {
		t.Errorf("verified copy reports %v, want a timestamp", verified.LastScrubbedAt)
	}

	never, ok := byBackend["backend-b"]
	if !ok {
		t.Fatalf("backend-b copy missing from %v", locs)
	}
	if never.LastScrubbedAt != nil {
		t.Errorf("never-verified copy reports %v, want nil", never.LastScrubbedAt)
	}
	if verified.ContentHash == "" || never.ContentHash == "" {
		t.Error("both copies should carry a hash, so the hash alone cannot tell them apart")
	}
}

// TestStoreInt_IntegrityCoverage_CountsNeverVerifiedCopies pins the figure the
// dashboard reads to the backlog rather than to the copies the sweep already
// reached. A copy with no scrub stamp is measured from when it was written, the
// same fallback the scrub queue orders on; taking MIN over the stamp alone skips
// it, and a fleet the sweep has never touched then reports an age of zero.
//
// The suite shares one database, so the assertion is a lower bound: other rows
// can only be younger than the backdated copy, so they cannot mask it.
func TestStoreInt_IntegrityCoverage_CountsNeverVerifiedCopies(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	reachable := []string{"backend-a", "backend-b"}

	key := uniqueKey(t, "unverified-age")
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()
	// Hashed at write rather than by backfill, which stamps, so the copy is
	// hashed and unverified: the state the coverage figures are about.
	if _, err := s.pool.Exec(ctx,
		`UPDATE object_locations SET content_hash = 'abc123', created_at = NOW() - INTERVAL '48 hours'
		 WHERE object_key = $1 AND backend_name = $2`, key, "backend-a",
	); err != nil {
		t.Fatalf("hashing at write and backdating created_at: %v", err)
	}

	stat, err := s.IntegrityCoverage(ctx, reachable)
	if err != nil {
		t.Fatalf("IntegrityCoverage: %v", err)
	}
	if stat.NeverVerified < 1 {
		t.Errorf("never verified = %d, want at least the copy just written", stat.NeverVerified)
	}
	if stat.OldestUnverifiedAge < 47*time.Hour {
		t.Errorf("age = %s, want at least the 48h-old never-verified copy", stat.OldestUnverifiedAge)
	}

	// Scoping the query away from the copy's backend moves it out of the age
	// and into the deferred count, which is what keeps an unreachable copy from
	// pinning a figure the sweep can never bring down.
	stat, err = s.IntegrityCoverage(ctx, []string{"backend-b"})
	if err != nil {
		t.Fatalf("IntegrityCoverage scoped away from backend-a: %v", err)
	}
	if stat.Deferred < 1 {
		t.Errorf("deferred = %d, want at least the copy on the excluded backend", stat.Deferred)
	}
	if stat.OldestUnverifiedAge >= 47*time.Hour {
		t.Errorf("age = %s, want the excluded copy left out", stat.OldestUnverifiedAge)
	}

	// Verifying it retires it from both figures.
	if err := s.MarkObjectScrubbed(ctx, key, "backend-a"); err != nil {
		t.Fatalf("MarkObjectScrubbed: %v", err)
	}
	stat, err = s.IntegrityCoverage(ctx, reachable)
	if err != nil {
		t.Fatalf("IntegrityCoverage after stamping: %v", err)
	}
	if stat.OldestUnverifiedAge >= 47*time.Hour {
		t.Errorf("age = %s, want the stamped copy to have left the head of the queue", stat.OldestUnverifiedAge)
	}
}

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
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// TEST HELPERS
// -------------------------------------------------------------------------

// testInstanceID is the instance every test store claims its queued cleanup
// rows for. testAbortReason and testDropReason are the labels the tests
// queue removed parts and copies under.
const (
	testInstanceID  = "instance-test"
	testAbortReason = "test_abort"
	testDropReason  = "test_drop"
)

// newTestStore creates an in-memory SQLite store for testing.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := NewStore(ctx, &config.DatabaseConfig{
		Driver: "sqlite",
		Path:   ":memory:",
	}, nil, testInstanceID)
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

// unstripeQuotaCounter puts the byte counter back in backend_quotas and drops
// the stripe table, undoing the striping migration so it can be replayed.
func unstripeQuotaCounter(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`ALTER TABLE backend_quotas ADD COLUMN bytes_used INTEGER NOT NULL DEFAULT 0`); err != nil {
		t.Fatalf("restore bytes_used column: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS backend_quota_stripes`); err != nil {
		t.Fatalf("drop quota stripes: %v", err)
	}
}

// rewindToSchemaVersion returns a test database to the shape it had at an
// earlier schema version. Stamping the version alone is not enough: schema.sql
// already has every current column, so re-running an additive migration would
// fail on a column that exists. Each migration rewound past needs its changes
// undone here.
func rewindToSchemaVersion(t *testing.T, s *Store, version int) {
	t.Helper()
	ctx := context.Background()

	for _, step := range schemaRewindSteps(t, s) {
		if version < step.version {
			step.undo()
		}
	}

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM schema_version; INSERT INTO schema_version (version) VALUES (?)`,
		version); err != nil {
		t.Fatalf("rewind schema version to %d: %v", version, err)
	}
}

// schemaRewindStep undoes what one migration added, so a store can be put back
// the way an installation predating that version would look.
type schemaRewindStep struct {
	version int
	undo    func()
}

// schemaRewindSteps lists every migration that left something behind, newest
// first. Rewinding to a version runs the steps above it.
func schemaRewindSteps(t *testing.T, s *Store) []schemaRewindStep {
	t.Helper()
	ctx := context.Background()
	exec := func(what, query string) {
		if _, err := s.db.ExecContext(ctx, query); err != nil {
			t.Fatalf("rewind: %s: %v", what, err)
		}
	}
	return []schemaRewindStep{
		{32, func() {
			exec("drop the pending outbox index", `DROP INDEX idx_notification_outbox_pending`)
			exec("restore the partial pending outbox index", `
				CREATE INDEX idx_notification_outbox_pending
				    ON notification_outbox(next_retry) WHERE attempts < 10`)
		}},
		{31, func() { dropColumns(t, s, "multipart_uploads", "completion_heartbeat_at") }},
		// The view reads multipart_parts, so it goes before the column; 0027
		// recreates it.
		{28, func() {
			exec("drop backend_capacity", `DROP VIEW IF EXISTS backend_capacity`)
			dropColumns(t, s, "multipart_parts", "storage_key")
		}},
		// The older views read the counter column, so it comes back for the
		// migrations that recreate them.
		{27, func() {
			exec("drop backend_capacity", `DROP VIEW IF EXISTS backend_capacity`)
			exec("restore the orphan counter", `ALTER TABLE backend_quotas ADD COLUMN orphan_bytes INTEGER NOT NULL DEFAULT 0`)
		}},
		// The generated columns read the stored-form columns, so they go before
		// any step that drops one of those.
		{26, func() { dropColumns(t, s, "object_locations", "form", "object_size") }},
		{25, func() {
			exec("drop the pending storage-key index", `DROP INDEX IF EXISTS idx_pending_objects_backend_storage_key`)
			exec("restore the pending backend index", `CREATE INDEX IF NOT EXISTS idx_pending_objects_backend ON pending_objects(backend_name)`)
		}},
		// The view reads the flags column, so it goes first; 0024 recreates it.
		{24, func() {
			exec("drop backend_capacity", `DROP VIEW IF EXISTS backend_capacity`)
			exec("restore the state column", `ALTER TABLE backend_states ADD COLUMN state TEXT`)
			exec("drop the flags column", `ALTER TABLE backend_states DROP COLUMN flags`)
		}},
		// The view reads the table, so it goes first; 0023 recreates it.
		{23, func() {
			exec("drop backend_capacity", `DROP VIEW IF EXISTS backend_capacity`)
			exec("rename backend_states back", `ALTER TABLE backend_states RENAME TO backend_drains`)
		}},
		{22, func() {
			exec("drop the drain paging index", `DROP INDEX IF EXISTS idx_object_locations_managed_size`)
			exec("drop the cleanup path index", `DROP INDEX IF EXISTS idx_cleanup_queue_backend_storage_key`)
			exec("drop the dlq path index", `DROP INDEX IF EXISTS idx_cleanup_dlq_backend_storage_key`)
			exec("restore the managed index", `CREATE INDEX IF NOT EXISTS idx_object_locations_managed ON object_locations(backend_name) WHERE managed`)
			exec("restore the backend index", `CREATE INDEX IF NOT EXISTS idx_object_locations_backend ON object_locations(backend_name)`)
			exec("restore the dlq backend index", `CREATE INDEX IF NOT EXISTS idx_cleanup_dlq_backend ON cleanup_dlq(backend_name)`)
		}},
		// The view goes before the storage-key columns: it reads the pending
		// table, and SQLite refuses to alter a table a view depends on.
		{20, func() {
			exec("drop backend_capacity", `DROP VIEW IF EXISTS backend_capacity`)
			exec("drop the storage-key index", `DROP INDEX IF EXISTS idx_object_locations_backend_storage_key`)
			for _, table := range []string{"object_locations", "pending_objects", "cleanup_queue", "cleanup_dlq"} {
				dropColumns(t, s, table, "storage_key")
			}
		}},
		{19, func() {
			exec("drop backend_drains", `DROP TABLE IF EXISTS backend_drains`)
		}},
		// Rebuilt rather than column-dropped: the primary key moved onto the
		// resource, and SQLite cannot take it back off in place any more than
		// the migration could put it on.
		{17, func() {
			exec("rewind grants to a bucket name", `
				CREATE TABLE grants_old (
				    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
				    bucket_name TEXT NOT NULL,
				    permissions TEXT NOT NULL DEFAULT '',
				    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
				    PRIMARY KEY (user_id, bucket_name)
				);
				INSERT INTO grants_old (user_id, bucket_name, permissions, created_at)
				SELECT user_id, resource_name, permissions, created_at
				FROM grants WHERE resource_kind = 'bucket';
				DROP TABLE grants;
				ALTER TABLE grants_old RENAME TO grants;`)
		}},
		{16, func() { dropColumns(t, s, "grants", "permissions") }},
		{14, func() {
			exec("drop pending key index", `DROP INDEX IF EXISTS idx_pending_objects_key`)
			dropColumns(t, s, "pending_objects", "role")
		}},
		{13, func() { unstripeQuotaCounter(t, s) }},
		{11, func() {
			for _, table := range []string{"object_locations", "pending_objects"} {
				dropColumns(t, s, table, "etag", "content_type", "user_metadata")
			}
			dropColumns(t, s, "multipart_parts", "plaintext_etag")
		}},
		{10, func() { dropColumns(t, s, "multipart_uploads", "tagging") }},
		{9, func() {
			// A table rather than a column, so the rewind drops the whole
			// thing: the migration under test creates it, and CREATE TABLE IF
			// NOT EXISTS would otherwise be a no-op against the baseline schema.
			exec("drop object_tags", `DROP TABLE IF EXISTS object_tags`)
		}},
		{8, func() {
			dropColumns(t, s, "object_locations", "compression_probe_size", "compression_probe_level")
		}},
		{7, func() {
			for _, table := range []string{"object_locations", "pending_objects"} {
				dropColumns(t, s, table, "compression_algorithm", "compression_level",
					"compression_format_version", "logical_size")
			}
		}},
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
	if _, _, err := s.RecordObject(context.Background(), &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: backend}}, Size: size}); err != nil {
		t.Fatalf("RecordObject(%s, %s): %v", key, backend, err)
	}
}

// mustRecordEncrypted records key on backend as an encrypted copy wrapped
// under keyID, occupying size bytes for a plaintext of plaintextSize.
func mustRecordEncrypted(t *testing.T, s *Store, key, backend, keyID string, plaintextSize, size int64) {
	t.Helper()
	if _, _, err := s.RecordObject(context.Background(), &core.RecordObjectRequest{
		Key: key, Copies: []core.ObjectCopy{{Backend: backend}}, Size: size,
		Form: &core.StoredForm{Encrypted: true, EncryptionKey: []byte("dek"), KeyID: keyID, PlaintextSize: plaintextSize},
	}); err != nil {
		t.Fatalf("RecordObject(%s, %s): %v", key, backend, err)
	}
}

// encryptedForm is the form an encryption pass places for a plaintext of
// plaintextSize wrapped under key-1.
func encryptedForm(plaintextSize int64) *core.StoredForm {
	return &core.StoredForm{Encrypted: true, EncryptionKey: []byte("dek"), KeyID: "key-1", PlaintextSize: plaintextSize}
}

// mustPatchCopy applies p to the copy of key on backend, failing the test on
// error.
func mustPatchCopy(t *testing.T, s *Store, key, backend string, p *core.CopyPatch) {
	t.Helper()
	if err := s.UpdateCopyMetadata(context.Background(), storetest.LocationOn(t, s, key, backend), p); err != nil {
		t.Fatalf("UpdateCopyMetadata(%s, %s): %v", key, backend, err)
	}
}

// mustSetContentHash records hash on the copy of key on backend and stamps it
// scrubbed, as the hash backfill does.
func mustSetContentHash(t *testing.T, s *Store, key, backend, hash string) {
	t.Helper()
	mustPatchCopy(t, s, key, backend, &core.CopyPatch{ContentHash: hash, Scrubbed: true})
}

// mustMarkScrubbed stamps the copy of key on backend as examined now.
func mustMarkScrubbed(t *testing.T, s *Store, key, backend string) {
	t.Helper()
	mustPatchCopy(t, s, key, backend, &core.CopyPatch{Scrubbed: true})
}

// seedBytesUsed writes a backend's counter directly, for the tests whose
// subject is the SQL that reads or moves bytes_used. Recording an object no
// longer touches it - a write reports its delta and the caller applies it to
// the in-memory tracker - so a seed has to say what it means.
func seedBytesUsed(t *testing.T, s *Store, backend string, used int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM backend_quota_stripes WHERE backend_name = ?`, backend); err != nil {
		t.Fatalf("clear stripes(%s): %v", backend, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO backend_quota_stripes (backend_name, stripe_id, bytes_used) VALUES (?, 0, ?)`,
		backend, used); err != nil {
		t.Fatalf("seed bytes_used(%s, %d): %v", backend, used, err)
	}
}

// mustCreateUpload creates a multipart upload, failing the test on error.
func mustCreateUpload(t *testing.T, s *Store, uploadID, key, backend string) {
	t.Helper()
	if created, err := s.CreateMultipartUpload(context.Background(), &core.CreateMultipartUploadParams{
		UploadID:    uploadID,
		ObjectKey:   key,
		BackendName: backend,
	}); err != nil || !created {
		t.Fatalf("CreateMultipartUpload(%s) = %v, %v", uploadID, created, err)
	}
}

// partStorageKey is where a test upload of part n carrying etag writes its
// bytes; the etag tells two uploads of one part number apart.
func partStorageKey(uploadID string, n int, etag string) string {
	return core.PartPath(uploadID, n) + "!" + etag
}

// mustRecordPart claims an intent for p on its upload's backend, the way an
// UploadPart claims before writing, then records the part under it. It
// returns the cleanup rows the record queued, failing the test on error.
func mustRecordPart(t *testing.T, s *Store, p *core.RecordPartParams) []core.CleanupItem {
	t.Helper()
	ctx := context.Background()
	mu, err := s.GetMultipartUpload(ctx, p.UploadID)
	if err != nil {
		t.Fatalf("GetMultipartUpload(%s): %v", p.UploadID, err)
	}
	p.Intent = mustClaimCopyIntent(t, s, core.PartPath(p.UploadID, p.PartNumber), mu.BackendName,
		partStorageKey(p.UploadID, p.PartNumber, p.ETag), p.SizeBytes, core.AcceptForPart)
	cleanup, err := s.RecordPart(ctx, p)
	if err != nil {
		t.Fatalf("RecordPart(%s, %d): %v", p.UploadID, p.PartNumber, err)
	}
	return cleanup
}

// mustClaimCopyIntent claims an extra-copy intent for size bytes of key at
// storageKey on backend, admitting it on a backend whose flags fall within
// accepts, the way a copy claims its target before writing. It fails the test
// unless the backend took the claim.
func mustClaimCopyIntent(t *testing.T, s *Store, key, backend, storageKey string, size int64, accepts core.BackendFlags) *core.PendingObject {
	t.Helper()
	p := &core.PendingObject{
		IntentID: "intent-" + backend + "-" + storageKey, ObjectKey: key, StorageKey: storageKey,
		BackendName: backend, SizeBytes: size, Role: core.PendingRoleCompanion,
	}
	fits, err := s.InsertPendingIfFits(context.Background(), p, accepts)
	if err != nil || !fits {
		t.Fatalf("InsertPendingIfFits(%s on %s) = %v, %v", storageKey, backend, fits, err)
	}
	return p
}

// placeOnto places the copy of key on source onto target at storageKey under
// a freshly claimed intent, removing the source when remove is set.
func placeOnto(t *testing.T, s *Store, key, source, target, storageKey string, remove bool) (int64, []core.CleanupItem, error) {
	t.Helper()
	src := storetest.LocationOn(t, s, key, source)
	return s.PlaceCopy(context.Background(), &core.Placement{
		Source:       src,
		Intent:       mustClaimCopyIntent(t, s, key, target, storageKey, src.SizeBytes, core.AcceptForWrite),
		RemoveSource: remove,
		Reasons:      testMoveReasons,
	})
}

// reformInPlace claims and places loc back on its own backend at storageKey
// with form and size, the way a rewrite pass records the bytes it rewrote.
func reformInPlace(t *testing.T, s *Store, loc *core.ObjectLocation, storageKey string, form *core.StoredForm, size int64) (int64, error) {
	t.Helper()
	intent := mustClaimCopyIntent(t, s, loc.ObjectKey, loc.BackendName, storageKey, size, core.AcceptForRewrite)
	intent.StoredForm = *form
	placed, _, err := s.PlaceCopy(context.Background(), &core.Placement{
		Source: loc, Intent: intent, RemoveSource: true, Reformed: true, Reasons: testMoveReasons,
	})
	return placed, err
}

// testMoveReasons are the labels the test placements queue bytes under.
var testMoveReasons = core.MoveReasons{Orphan: "test_orphan", SourceDelete: "test_source_delete"}

// mustRecordReplica places a replica of the copy on source onto target,
// failing the test on error. The size parameter only documents the expected
// size at the call site; the intent takes it from the source row.
func mustRecordReplica(t *testing.T, s *Store, key, target, source string, _ int64) {
	t.Helper()
	if _, _, err := placeOnto(t, s, key, source, target, key+"!r-"+target, false); err != nil {
		t.Fatalf("PlaceCopy(%s, %s): %v", key, target, err)
	}
}

// mustMoveCopy moves the copy of key on source onto target and returns the
// moved size, failing the test on error.
func mustMoveCopy(t *testing.T, s *Store, key, source, target string) int64 {
	t.Helper()
	moved, _, err := placeOnto(t, s, key, source, target, key+"!m-"+target, true)
	if err != nil {
		t.Fatalf("PlaceCopy move(%s, %s -> %s): %v", key, source, target, err)
	}
	return moved
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

	displaced, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/key1", Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 1024})
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
	displaced, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/key1", Copies: []core.ObjectCopy{{Backend: "backend-b"}}, Size: 2048})
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

	deleted, _, err := s.DeleteObject(ctx, "bucket/key1")
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if len(deleted) != 1 {
		t.Errorf("expected 1 deleted copy, got %d", len(deleted))
	}

	_, err = s.GetAllObjectLocations(ctx, "bucket/key1")
	if !errors.Is(err, core.ErrObjectNotFound) {
		t.Errorf("expected ErrObjectNotFound, got %v", err)
	}
}

// TestDeleteObject_NotFound verifies that deleting a nonexistent object returns ErrObjectNotFound.
func TestDeleteObject_NotFound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	_, _, err := s.DeleteObject(ctx, "bucket/nonexistent")
	if !errors.Is(err, core.ErrObjectNotFound) {
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

// TestListObjects_ReportsLogicalSizes verifies S3-facing listings expose the
// client-visible object length rather than the physical encoded/encrypted byte
// count used for backend quota accounting.
func TestListObjects_ReportsLogicalSizes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	record := func(key string, storedSize int64, form *core.StoredForm) {
		t.Helper()
		if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
			Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: storedSize, Form: form,
		}); err != nil {
			t.Fatalf("RecordObject(%s): %v", key, err)
		}
	}

	record("bucket/plain", 80, nil)
	record("bucket/encrypted", 120, &core.StoredForm{
		Encrypted: true, PlaintextSize: 100, EncryptionKey: []byte{1}, KeyID: "k1",
	})
	record("bucket/compressed", 40, &core.StoredForm{
		CompressionAlgorithm: "zstd-seekable", LogicalSize: 100,
	})
	record("bucket/both", 70, &core.StoredForm{
		Encrypted: true, PlaintextSize: 40, EncryptionKey: []byte{1}, KeyID: "k1",
		CompressionAlgorithm: "zstd-seekable", LogicalSize: 100,
	})

	assertSizes := func(t *testing.T, objects []core.ObjectLocation) {
		t.Helper()
		got := make(map[string]int64, len(objects))
		for _, obj := range objects {
			got[obj.ObjectKey] = obj.SizeBytes
		}
		want := map[string]int64{
			"bucket/plain": 80, "bucket/encrypted": 100,
			"bucket/compressed": 100, "bucket/both": 100,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("listed sizes = %#v, want %#v", got, want)
		}
	}

	flat, err := s.ListObjects(ctx, "bucket/", "", 10)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	assertSizes(t, flat.Objects)

	delimited, err := s.ListObjectsDelimited(ctx, "bucket/", "/", "", 10)
	if err != nil {
		t.Fatalf("ListObjectsDelimited: %v", err)
	}
	assertSizes(t, delimited.Objects)
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

// TestListObjectsByBackend verifies the listing returns only the backend's
// objects, smallest first with the key breaking ties, and that the cursor
// resumes after the last (size, key) it returned.
func TestListObjectsByBackend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/c", "backend-a", 200)
	mustRecordObject(t, s, "bucket/b", "backend-a", 100)
	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/other", "backend-b", 50)

	keys := func(after core.SizeCursor) []string {
		t.Helper()
		locs, err := s.ListObjectsByBackend(ctx, "backend-a", 2, after)
		if err != nil {
			t.Fatalf("ListObjectsByBackend: %v", err)
		}
		out := make([]string, len(locs))
		for i := range locs {
			out[i] = locs[i].ObjectKey
		}
		return out
	}
	if got := keys(core.SizeCursor{}); !slices.Equal(got, []string{"bucket/a", "bucket/b"}) {
		t.Fatalf("first page = %v, want a then b at the same size", got)
	}
	if got := keys(core.SizeCursor{SizeBytes: 100, ObjectKey: "bucket/b"}); !slices.Equal(got, []string{"bucket/c"}) {
		t.Errorf("page after (100, b) = %v, want only c", got)
	}
}

// TestListedPathStates verifies one lookup reports every state a listed path
// can be in on the backend asked about: a recorded copy, an in-flight write intent,
// a queued delete, a dead-lettered delete, a path both recorded and queued, and a path
// with both an intent and a queued delete, where the pending delete wins. Copies on
// another backend and unknown paths are absent.
func TestListedPathStates(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustEnqueueCleanup(t, s, "backend-a", "bucket/dlq")
	dlqAll(t, s, "gave up")
	mustRecordObject(t, s, "bucket/rec", "backend-a", 10)
	mustRecordObject(t, s, "bucket/both", "backend-a", 10)
	mustRecordObject(t, s, "bucket/other", "backend-b", 10)
	mustEnqueueCleanup(t, s, "backend-a", "bucket/queued")
	mustEnqueueCleanup(t, s, "backend-a", "bucket/both")

	if fits, err := s.InsertPendingIfFits(ctx, &core.PendingObject{
		IntentID:    "intent-only",
		ObjectKey:   "bucket/intent-only",
		StorageKey:  "bucket/intent-only",
		BackendName: "backend-a",
		SizeBytes:   10,
	}, core.AcceptForWrite); err != nil || !fits {
		t.Fatalf("InsertPendingIfFits(intent-only): fits=%v, err=%v", fits, err)
	}

	if fits, err := s.InsertPendingIfFits(ctx, &core.PendingObject{
		IntentID:    "intent-queued",
		ObjectKey:   "bucket/intent-queued",
		StorageKey:  "bucket/intent-queued",
		BackendName: "backend-a",
		SizeBytes:   10,
	}, core.AcceptForWrite); err != nil || !fits {
		t.Fatalf("InsertPendingIfFits(intent-queued): fits=%v, err=%v", fits, err)
	}
	mustEnqueueCleanup(t, s, "backend-a", "bucket/intent-queued")

	got, err := s.ListedPathStates(ctx, "backend-a",
		[]string{"bucket/rec", "bucket/intent-only", "bucket/intent-queued", "bucket/queued", "bucket/dlq", "bucket/both", "bucket/other", "bucket/absent"})
	if err != nil {
		t.Fatalf("ListedPathStates: %v", err)
	}
	want := map[string]core.PathState{
		"bucket/rec":           core.PathRecorded,
		"bucket/intent-only":   core.PathInFlight,
		"bucket/intent-queued": core.PathPendingCleanup,
		"bucket/queued":        core.PathPendingCleanup,
		"bucket/dlq":           core.PathPendingCleanup,
		"bucket/both":          core.PathPendingCleanup,
	}
	if !maps.Equal(got, want) {
		t.Errorf("states = %v, want %v", got, want)
	}

	if empty, err := s.ListedPathStates(ctx, "backend-a", nil); err != nil || len(empty) != 0 {
		t.Errorf("no paths = %v, %v; want an empty map", empty, err)
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

// The listing stream reconcile deletes from must carry committed rows only.
// An intent whose upload has not landed must never appear in it, or the merge
// would delete the object's committed copy.
func TestListObjectsByBackendKeyAsc_ExcludesPendingIntents(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	// committed copy at k!old, plus an intent for k!new with no committed row
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key:    "bucket/k",
		Size:   500,
		Copies: []core.ObjectCopy{{Backend: "backend-a", StorageKey: "bucket/k!old"}},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	if fits, err := s.InsertPendingIfFits(ctx, &core.PendingObject{
		IntentID:    "intent-new",
		ObjectKey:   "bucket/k",
		StorageKey:  "bucket/k!new",
		BackendName: "backend-a",
		SizeBytes:   600,
	}, core.AcceptForWrite); err != nil || !fits {
		t.Fatalf("InsertPendingIfFits: fits=%v, err=%v", fits, err)
	}

	got, err := s.ListObjectsByBackendKeyAsc(ctx, "backend-a", "", 10)
	if err != nil {
		t.Fatalf("ListObjectsByBackendKeyAsc: %v", err)
	}
	if len(got) != 1 || got[0].StorageKey != "bucket/k!old" {
		t.Errorf("got %+v, want only the committed copy", got)
	}
}

// TestImportObject verifies that importing a pre-existing object records it correctly.
func TestImportObject(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	outcome, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: "bucket/new", Backend: "backend-a", Size: 500})
	if err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	if outcome != core.ImportInserted {
		t.Errorf("outcome = %s, want inserted for a new object", outcome)
	}

	// Import again should be a no-op
	outcome, err = s.ImportObject(ctx, &core.ImportObjectRequest{Key: "bucket/new", Backend: "backend-a", Size: 500})
	if err != nil {
		t.Fatalf("ImportObject duplicate: %v", err)
	}
	if outcome != core.ImportSkippedExisting {
		t.Errorf("outcome = %s, want skipped_existing for a duplicate", outcome)
	}
}

// TestImportObject_SuppressedByPendingCleanup verifies a key whose delete is
// still outstanding is not imported back. Without this, a delete that could
// not reach a backend is undone the next time reconcile walks it: the object
// returns live, the replicator spreads it, and its created_at restarts so a
// lifecycle rule that expired it waits another full window.
func TestImportObject_SuppressedByPendingCleanup(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	const (
		key     = "bucket/deleted"
		backend = "backend-a"
	)

	mustRecordObject(t, s, key, backend, 500)
	if _, _, err := s.DeleteObject(ctx, key); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	outcome, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: key, Backend: backend, Size: 500})
	if err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	if outcome != core.ImportSkippedPendingCleanup {
		t.Errorf("outcome = %s, want skipped_pending_cleanup", outcome)
	}

	if _, err := s.GetAllObjectLocations(ctx, key); !errors.Is(err, core.ErrObjectNotFound) {
		t.Errorf("ledger error = %v, want ErrObjectNotFound for a suppressed import", err)
	}
}

// TestImportObject_SuppressedByInFlightIntent verifies a path held only by an
// in-flight PUT intent is not imported as a pre-existing object.
func TestImportObject_SuppressedByInFlightIntent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	const (
		key     = "bucket/in-flight"
		backend = "backend-a"
	)

	p := core.PendingObject{
		IntentID:    "intent-import-skip",
		ObjectKey:   key,
		StorageKey:  key,
		BackendName: backend,
		SizeBytes:   500,
	}
	if fits, err := s.InsertPendingIfFits(ctx, &p, core.AcceptForWrite); err != nil || !fits {
		t.Fatalf("InsertPendingIfFits: fits=%v err=%v", fits, err)
	}

	outcome, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: key, Backend: backend, Size: 500})
	if err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	if outcome != core.ImportSkippedExisting {
		t.Errorf("outcome = %s, want skipped_existing", outcome)
	}

	if _, err := s.GetAllObjectLocations(ctx, key); !errors.Is(err, core.ErrObjectNotFound) {
		t.Errorf("ledger error = %v, want ErrObjectNotFound for an in-flight path", err)
	}
}

// TestImportObject_SuppressedByDeadLetteredCleanup verifies the suppression
// also covers a cleanup that exhausted its retries. Retrying stopped because
// the backend would not take the delete, not because the delete was withdrawn,
// so the bytes are still an orphan awaiting an operator.
func TestImportObject_SuppressedByDeadLetteredCleanup(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	const (
		key     = "bucket/dead-lettered"
		backend = "backend-a"
	)

	item := queueCleanup(t, s, backend, key, 500)
	if moved, err := s.MoveCleanupToDLQ(ctx, &item, "gave up"); err != nil || !moved {
		t.Fatalf("MoveCleanupToDLQ = %v, %v", moved, err)
	}

	outcome, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: key, Backend: backend, Size: 500})
	if err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	if outcome != core.ImportSkippedPendingCleanup {
		t.Errorf("outcome = %s, want skipped_pending_cleanup", outcome)
	}
}

// TestImportObject_OtherBackendUnaffected verifies suppression is scoped to
// the backend the delete is outstanding on. A copy that was removed cleanly
// elsewhere must still be importable, or one stuck cleanup would block the
// whole key from ever being reconciled.
func TestImportObject_OtherBackendUnaffected(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	const key = "bucket/partial"

	queueCleanup(t, s, "backend-a", key, 500)

	outcome, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: key, Backend: "backend-b", Size: 500})
	if err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	if outcome != core.ImportInserted {
		t.Errorf("outcome = %s, want inserted on a backend with no pending delete", outcome)
	}
}

// TestRecordObject_Overwrite_SameBackend covers an overwrite landing on the
// backend that already holds the object. The prior copy is displaced at its
// own path: the new write stores its bytes elsewhere, and the overwrite queues
// the old path for deletion, claimed by this instance, in its own
// transaction.
func TestRecordObject_Overwrite_SameBackend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k", Size: 500,
		Copies: []core.ObjectCopy{{Backend: "backend-a", StorageKey: "bucket/k!first"}},
	}); err != nil {
		t.Fatalf("first RecordObject: %v", err)
	}

	displaced, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k", Size: 700,
		Copies: []core.ObjectCopy{{Backend: "backend-a", StorageKey: "bucket/k!second"}},
	})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	assertQueuedOnly(t, s, displaced, "backend-a", "bucket/k!first", core.CleanupReasonOverwritten, 500)
	if got := orphanBytes(t, s, "backend-a"); got != 500 {
		t.Errorf("orphan_bytes = %d, want the displaced copy's 500", got)
	}
}

// TestDeleteObject_QueuesEveryCopy verifies a delete queues the bytes of
// every copy it removes under the delete reason, and the rows count as the
// backends' orphans until they are retired.
func TestDeleteObject_QueuesEveryCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecordObject(t, s, "bucket/gone", "backend-a", 300)
	mustRecordReplica(t, s, "bucket/gone", "backend-b", "backend-a", 300)

	cleanup, _, err := s.DeleteObject(ctx, "bucket/gone")
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if len(cleanup) != 2 {
		t.Fatalf("cleanup = %+v, want one row per copy", cleanup)
	}
	for _, c := range cleanup {
		if c.Reason != core.CleanupReasonDeleted || c.SizeBytes != 300 || c.ObjectKey != "bucket/gone" {
			t.Errorf("cleanup row = %+v, want 300 bytes of bucket/gone under %q", c, core.CleanupReasonDeleted)
		}
	}
	if depth, _ := s.CleanupQueueDepth(ctx); depth != 2 {
		t.Errorf("queue depth = %d, want 2", depth)
	}
	for _, backend := range []string{"backend-a", "backend-b"} {
		if got := orphanBytes(t, s, backend); got != 300 {
			t.Errorf("orphan_bytes(%s) = %d, want 300", backend, got)
		}
	}
}

// TestRecordObject_ReportsDeltaForTheWrite pins what replaced the in-transaction
// quota guard: the write reports the bytes it added, and the caller applies them
// to the counter the limit is enforced against.
func TestRecordObject_ReportsDeltaForTheWrite(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	_, deltas, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/new", Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 500,
	})
	if err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if deltas["backend-a"] != 500 {
		t.Errorf("delta = %d, want 500", deltas["backend-a"])
	}
}

// TestRecordObject_OverwriteNetsTheDelta covers the overwrite arithmetic the
// caller depends on: the replaced copy's bytes come off in the same map the new
// copy's go on, so a same-size overwrite moves the counter by nothing.
func TestRecordObject_OverwriteNetsTheDelta(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k", Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 500,
	}); err != nil {
		t.Fatalf("seed RecordObject: %v", err)
	}
	_, deltas, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k", Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 800,
	})
	if err != nil {
		t.Fatalf("overwrite RecordObject: %v", err)
	}
	if deltas["backend-a"] != 300 {
		t.Errorf("delta = %d, want 300 (800 written less 500 replaced)", deltas["backend-a"])
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
	_, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/encrypted", Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 1100, Form: form})
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

// TestListBackendQuotaUsage_ReportsCeilingAndOccupancy verifies the baseline
// read the quota tracker is primed from: the limit, the counter, and the bytes
// on the backend that the counter does not carry. Placement is judged against
// these three, so a column missing here is a backend that reads emptier than it
// is.
func TestListBackendQuotaUsage_ReportsCeilingAndOccupancy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.db.ExecContext(ctx,
		`UPDATE backend_quotas SET bytes_limit = 1000 WHERE backend_name = 'backend-a'`); err != nil {
		t.Fatalf("seed quota row: %v", err)
	}
	seedBytesUsed(t, s, "backend-a", 400)
	queueCleanup(t, s, "backend-a", "bucket/orphan", 100)

	usage, err := s.ListBackendQuotaUsage(ctx)
	if err != nil {
		t.Fatalf("ListBackendQuotaUsage: %v", err)
	}
	var got core.BackendQuotaUsage
	for _, u := range usage {
		if u.BackendName == "backend-a" {
			got = u
		}
	}
	if got.BytesLimit != 1000 || got.BytesUsed != 400 || got.OrphanBytes != 100 {
		t.Fatalf("usage = %+v, want limit 1000, used 400, orphan 100", got)
	}
	if got.Occupied() != 500 {
		t.Errorf("Occupied() = %d, want 500 (used + orphan + inflight)", got.Occupied())
	}
}

// TestListBackendQuotaUsage_CountsInflightParts asserts the parts of an upload
// that has not completed are reported as occupancy. They are on the backend
// without being in bytes_used, and a baseline that ignored them would admit
// writes into space already spoken for.
func TestListBackendQuotaUsage_CountsInflightParts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateMultipartUpload(ctx, &core.CreateMultipartUploadParams{
		UploadID:    "upload-inflight",
		ObjectKey:   "bucket/big",
		BackendName: "backend-a",
	}); err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	mustRecordPart(t, s, &core.RecordPartParams{
		UploadID:   "upload-inflight",
		PartNumber: 1,
		ETag:       `"p1"`,
		SizeBytes:  777,
	})

	usage, err := s.ListBackendQuotaUsage(ctx)
	if err != nil {
		t.Fatalf("ListBackendQuotaUsage: %v", err)
	}
	for _, u := range usage {
		if u.BackendName == "backend-a" {
			if u.InflightBytes != 777 {
				t.Errorf("InflightBytes = %d, want 777", u.InflightBytes)
			}
			return
		}
	}
	t.Fatal("backend-a absent from the usage listing")
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

// TestOrphanBytes verifies a backend's orphan bytes are its queued cleanup
// rows, read through GetQuotaStats.
func TestOrphanBytes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	queueCleanup(t, s, "backend-a", "bucket/orphan", 500)

	stats, _ := s.GetQuotaStats(context.Background())
	if stats["backend-a"].OrphanBytes != 500 {
		t.Errorf("orphan_bytes = %d, want 500", stats["backend-a"].OrphanBytes)
	}
	if stats["backend-b"].OrphanBytes != 0 {
		t.Errorf("backend-b orphan_bytes = %d, want 0", stats["backend-b"].OrphanBytes)
	}
}

// -------------------------------------------------------------------------
// MULTIPART UPLOADS
// -------------------------------------------------------------------------

// TestListParts_PagesAfterMarker returns parts above the marker in part-number
// order, at most limit of them, whatever order they were recorded in.
func TestListParts_PagesAfterMarker(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateMultipartUpload(ctx, &core.CreateMultipartUploadParams{
		UploadID: "upload-pages", ObjectKey: "bucket/big.bin", BackendName: "backend-a",
	}); err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	for _, n := range []int{5, 1, 3, 2, 4} {
		mustRecordPart(t, s, &core.RecordPartParams{UploadID: "upload-pages", PartNumber: n, ETag: "e", SizeBytes: 1})
	}

	for _, tc := range []struct {
		after, limit int
		want         []int
	}{
		{0, 2, []int{1, 2}},
		{2, 2, []int{3, 4}},
		{4, 2, []int{5}},
		{5, 2, []int{}},
	} {
		parts, err := s.ListParts(ctx, "upload-pages", tc.after, tc.limit)
		if err != nil {
			t.Fatalf("ListParts(after %d): %v", tc.after, err)
		}
		got := make([]int, 0, len(parts))
		for _, p := range parts {
			got = append(got, p.PartNumber)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("ListParts(after %d, limit %d) = %v, want %v", tc.after, tc.limit, got, tc.want)
		}
	}
}

// TestMultipartUpload_Lifecycle verifies the full create/record-part/complete/delete lifecycle.
func TestMultipartUpload_Lifecycle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	meta := map[string]string{"Content-Type": "image/png"}
	_, err := s.CreateMultipartUpload(ctx, &core.CreateMultipartUploadParams{
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
	for n, size := range map[int]int64{1: 1024, 2: 2048} {
		if cleanup := mustRecordPart(t, s, &core.RecordPartParams{
			UploadID: "upload-1", PartNumber: n, ETag: fmt.Sprintf("etag%d", n), SizeBytes: size,
		}); len(cleanup) != 0 {
			t.Errorf("RecordPart(%d) queued %+v, want nothing for a first upload", n, cleanup)
		}
	}
	if n := queryPendingCount(t, s); n != 0 {
		t.Errorf("pending intents = %d, want every part's intent cleared", n)
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
	cleanup, err := s.DeleteMultipartUpload(ctx, "upload-1", testAbortReason, time.Now())
	if err != nil {
		t.Fatalf("DeleteMultipartUpload: %v", err)
	}
	assertPartsQueued(t, cleanup, "upload-1", map[int]int64{1: 1024, 2: 2048})

	_, err = s.GetMultipartUpload(ctx, "upload-1")
	if !errors.Is(err, core.ErrMultipartUploadNotFound) {
		t.Errorf("expected ErrMultipartUploadNotFound, got %v", err)
	}
	again, err := s.DeleteMultipartUpload(ctx, "upload-1", testAbortReason, time.Now())
	if err != nil || len(again) != 0 {
		t.Errorf("DeleteMultipartUpload of a gone upload = %+v, %v; want nothing queued", again, err)
	}
}

// assertPartsQueued checks cleanup holds one row per part in sizes, each at
// the path the part's upload wrote on backend-a under the test abort reason.
func assertPartsQueued(t *testing.T, cleanup []core.CleanupItem, uploadID string, sizes map[int]int64) {
	t.Helper()
	if len(cleanup) != len(sizes) {
		t.Fatalf("cleanup = %+v, want one row per part", cleanup)
	}
	want := map[string]int64{}
	for n, size := range sizes {
		want[partStorageKey(uploadID, n, fmt.Sprintf("etag%d", n))] = size
	}
	for _, c := range cleanup {
		if size, ok := want[c.StorageKey]; !ok || c.SizeBytes != size || c.Reason != testAbortReason || c.BackendName != "backend-a" {
			t.Errorf("cleanup row = %+v, want one of %v on backend-a under test_abort", c, want)
		}
	}
}

// TestRecordPart_ReuploadQueuesReplacedPart verifies a second upload of a
// part number records the new path and queues the replaced part's bytes at
// the path its own upload wrote.
func TestRecordPart_ReuploadQueuesReplacedPart(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	const uploadID = "upload-re"
	mustCreateUpload(t, s, uploadID, "bucket/re", "backend-a")
	mustRecordPart(t, s, &core.RecordPartParams{UploadID: uploadID, PartNumber: 1, ETag: "first", SizeBytes: 100})

	cleanup := mustRecordPart(t, s, &core.RecordPartParams{UploadID: uploadID, PartNumber: 1, ETag: "second", SizeBytes: 150})
	assertQueuedOnly(t, s, cleanup, "backend-a", partStorageKey(uploadID, 1, "first"), core.CleanupReasonPartReplaced, 100)

	parts, err := s.GetParts(context.Background(), uploadID)
	if err != nil {
		t.Fatalf("GetParts: %v", err)
	}
	if len(parts) != 1 || parts[0].StorageKey != partStorageKey(uploadID, 1, "second") || parts[0].SizeBytes != 150 {
		t.Errorf("parts = %+v, want the second upload at its own path", parts)
	}
}

// TestRecordPart_LostIntentQueuesTheUpload verifies a part whose intent was
// resolved while it uploaded is refused with ErrIntentLost, records nothing,
// and queues the bytes the upload wrote.
func TestRecordPart_LostIntentQueuesTheUpload(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const uploadID = "upload-lost"
	mustCreateUpload(t, s, uploadID, "bucket/lost", "backend-a")
	path := core.PartPath(uploadID, 1)
	written := path + "!lost"
	intent := mustClaimCopyIntent(t, s, path, "backend-a", written, 64, core.AcceptForPart)
	if err := s.DeletePending(ctx, intent.IntentID); err != nil {
		t.Fatalf("DeletePending: %v", err)
	}

	cleanup, err := s.RecordPart(ctx, &core.RecordPartParams{
		UploadID: uploadID, PartNumber: 1, ETag: "e", SizeBytes: 64, Intent: intent,
	})
	if !errors.Is(err, core.ErrIntentLost) {
		t.Fatalf("RecordPart = %v, want ErrIntentLost", err)
	}
	assertQueuedOnly(t, s, cleanup, "backend-a", written, core.CleanupReasonSupersededIntent, 64)
	if parts, _ := s.GetParts(ctx, uploadID); len(parts) != 0 {
		t.Errorf("parts = %+v, want none recorded", parts)
	}
}

// TestRecordPart_RejectsPartNumberOutOfRange verifies a part number outside
// the S3 range is refused before any transaction runs.
func TestRecordPart_RejectsPartNumberOutOfRange(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, n := range []int{0, 10001} {
		if _, err := s.RecordPart(context.Background(), &core.RecordPartParams{UploadID: "u", PartNumber: n}); err == nil {
			t.Errorf("RecordPart(part %d) succeeded, want a range error", n)
		}
	}
}

// TestUpsertPart_ReturnsReplacedPart verifies the adapter returns nil for a
// first record of a part number and the prior row, with its storage key, for
// a second, and stores each record's storage key.
func TestUpsertPart_ReturnsReplacedPart(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const uploadID = "upload-up"
	mustCreateUpload(t, s, uploadID, "bucket/up", "backend-a")
	a := s.direct()
	first, err := a.UpsertPart(ctx, &core.RecordPartParams{
		UploadID: uploadID, PartNumber: 2, ETag: "a", SizeBytes: 10,
		Intent: &core.PendingObject{StorageKey: "path-a"},
	})
	if err != nil || first != nil {
		t.Fatalf("first UpsertPart = %+v, %v; want nil replaced", first, err)
	}
	second, err := a.UpsertPart(ctx, &core.RecordPartParams{
		UploadID: uploadID, PartNumber: 2, ETag: "b", SizeBytes: 20,
		Form:   encryptedForm(18),
		Intent: &core.PendingObject{StorageKey: "path-b"},
	})
	if err != nil {
		t.Fatalf("second UpsertPart: %v", err)
	}
	if second == nil || second.PartNumber != 2 || second.SizeBytes != 10 || second.StorageKey != "path-a" {
		t.Errorf("replaced = %+v, want part 2 of 10 bytes at path-a", second)
	}
	parts, err := s.GetParts(ctx, uploadID)
	if err != nil {
		t.Fatalf("GetParts: %v", err)
	}
	if len(parts) != 1 || parts[0].StorageKey != "path-b" || !parts[0].Encrypted || parts[0].PlaintextSize != 18 {
		t.Errorf("parts = %+v, want the encrypted second record at path-b", parts)
	}
}

// TestDeleteMultipartUpload_LegacyPartAtFixedPath verifies a part recorded
// with no storage key is queued at the part number's fixed path.
func TestDeleteMultipartUpload_LegacyPartAtFixedPath(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const uploadID = "upload-legacy"
	mustCreateUpload(t, s, uploadID, "bucket/legacy", "backend-b")
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO multipart_parts (upload_id, part_number, etag, size_bytes) VALUES (?, 3, 'e', 42)`,
		uploadID); err != nil {
		t.Fatalf("seed legacy part: %v", err)
	}

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		backend, heartbeat, err := a.LockMultipartUpload(ctx, uploadID)
		if err != nil || backend != "backend-b" || !heartbeat.IsZero() {
			t.Fatalf("LockMultipartUpload = %s, %v, %v; want backend-b with no mark", backend, heartbeat, err)
		}
		parts, err := a.DeleteMultipartUpload(ctx, uploadID)
		if err != nil {
			t.Fatalf("DeleteMultipartUpload: %v", err)
		}
		if len(parts) != 1 || parts[0].StorageKey != "" || parts[0].SizeBytes != 42 {
			t.Errorf("DeleteMultipartUpload = %+v; want the one legacy part", parts)
		}
	})
	cleanup, err := s.DeleteMultipartUpload(ctx, uploadID, testAbortReason, time.Now())
	if err != nil {
		t.Fatalf("DeleteMultipartUpload: %v", err)
	}
	assertQueuedOnly(t, s, cleanup, "backend-b", core.PartPath(uploadID, 3), testAbortReason, 42)
}

// TestMultipartCompletionMark verifies the completion mark holds off a second
// completion, part records and upload deletes until it is cleared or stale.
func TestMultipartCompletionMark(t *testing.T) {
	t.Parallel()
	storetest.CheckCompletionMark(t, newTestStore(t), "upload-mark", "backend-a")
}

// TestMultipartCompletionMark_ClosedStoreErrors verifies every completion mark
// operation returns its error from a closed store.
func TestMultipartCompletionMark_ClosedStoreErrors(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	s.Close()
	ctx := context.Background()
	if _, err := s.BeginMultipartCompletion(ctx, "u", time.Now()); err == nil {
		t.Error("BeginMultipartCompletion: expected an error")
	}
	if _, err := s.RenewMultipartCompletion(ctx, "u"); err == nil {
		t.Error("RenewMultipartCompletion: expected an error")
	}
	if err := s.EndMultipartCompletion(ctx, "u"); err == nil {
		t.Error("EndMultipartCompletion: expected an error")
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
	size, _, err := placeOnto(t, s, "bucket/key1", "backend-a", "backend-b", "bucket/key1!r-backend-b", false)
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
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

// TestReplication_ALiveIntentCountsWhateverItsRole verifies a key whose write
// is still placing copies is not under-replicated whichever of its intents is
// the one still uploading. The first copy to land records the object, and when
// that is a companion the write's primary intent is the one in flight; a count
// of companion intents alone read such a key as one copy short.
func TestReplication_ALiveIntentCountsWhateverItsRole(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{
		{Name: "backend-a", QuotaBytes: 1 << 30},
		{Name: "backend-b", QuotaBytes: 1 << 30},
		{Name: "backend-c", QuotaBytes: 1 << 30},
	}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}

	primary := core.PendingObject{IntentID: "i-primary", ObjectKey: "bucket/k", BackendName: "backend-b", SizeBytes: 1024}
	companion := core.PendingObject{IntentID: "i-companion", ObjectKey: "bucket/k", BackendName: "backend-c", SizeBytes: 1024, Role: core.PendingRoleCompanion}
	for _, p := range []*core.PendingObject{&primary, &companion} {
		if fits, err := s.InsertPendingIfFits(ctx, p, core.AcceptForWrite); err != nil || !fits {
			t.Fatalf("InsertPendingIfFits %s: fits=%v err=%v", p.IntentID, fits, err)
		}
	}
	// A companion on backend-a landed first and recorded the object; the
	// primary and the other companion are still uploading.
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k", Size: 1024,
		Copies:  []core.ObjectCopy{{Backend: "backend-a"}},
		Placing: []core.ObjectCopy{{Backend: "backend-b", IntentID: "i-primary"}, {Backend: "backend-c", IntentID: "i-companion"}},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	under, err := s.GetUnderReplicatedObjects(ctx, 3, 10)
	if err != nil {
		t.Fatalf("GetUnderReplicatedObjects: %v", err)
	}
	if len(under) != 0 {
		t.Errorf("under-replicated rows = %d, want 0: one copy recorded and two intents live meet factor 3", len(under))
	}
	under, err = s.GetUnderReplicatedObjectsExcluding(ctx, 3, 10, []string{"backend-d"})
	if err != nil {
		t.Fatalf("GetUnderReplicatedObjectsExcluding: %v", err)
	}
	if len(under) != 0 {
		t.Errorf("under-replicated rows (excluding) = %d, want 0", len(under))
	}
}

// TestRemoveExcessCopy verifies a surplus copy is removed while the object
// holds more copies than keepAtLeast, and its bytes are queued under the
// caller's reason.
func TestRemoveExcessCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/key1", "backend-a", 1024)
	mustRecordReplica(t, s, "bucket/key1", "backend-b", "backend-a", 1024)

	cleanup, err := s.DeleteObjectLocation(ctx, storetest.LocationOn(t, s, "bucket/key1", "backend-b"), 1, testDropReason)
	if err != nil {
		t.Fatalf("DeleteObjectLocation: %v", err)
	}
	assertQueuedOnly(t, s, cleanup, "backend-b", "bucket/key1!r-backend-b", testDropReason, 1024)

	locs, _ := s.GetAllObjectLocations(ctx, "bucket/key1")
	if len(locs) != 1 {
		t.Errorf("expected 1 copy after removal, got %d", len(locs))
	}
}

// TestRemoveCopy_EmptyReasonQueuesNothing verifies a removal with no reason
// drops the row only, for bytes the caller knows are already gone.
func TestRemoveCopy_EmptyReasonQueuesNothing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecordObject(t, s, "bucket/vanished", "backend-a", 64)

	cleanup, err := s.DeleteObjectLocation(ctx, storetest.LocationOn(t, s, "bucket/vanished", "backend-a"), 0, "")
	if err != nil {
		t.Fatalf("DeleteObjectLocation: %v", err)
	}
	if len(cleanup) != 0 || len(queuedCleanups(t, s)) != 0 {
		t.Errorf("cleanup = %+v, want nothing queued without a reason", cleanup)
	}
	if n := queryObjectLocationsCount(t, s, "bucket/vanished"); n != 0 {
		t.Errorf("rows left = %d, want the copy removed", n)
	}
}

// TestRemoveExcessCopy_NoOpWhenAtFactor covers the race where another path
// has already brought the copy count down to factor before this tx takes the
// lock. The in-tx re-read sees count == factor and deletes nothing, so the
// object is not left under-replicated.
func TestRemoveExcessCopy_NoOpWhenAtFactor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/k-atfactor", "backend-a", 1024)

	_, err := s.DeleteObjectLocation(ctx, storetest.LocationOn(t, s, "bucket/k-atfactor", "backend-a"), 1, testDropReason)
	if !errors.Is(err, core.ErrCopyNotExcess) {
		t.Fatalf("expected ErrCopyNotExcess when count==factor, got %v", err)
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
	victim := storetest.LocationOn(t, s, "bucket/k-vgone", "backend-b")

	// Simulate a parallel client landing the delete on backend-b
	// before the cleaner's per-victim tx executes.
	if _, err := s.DeleteObjectLocation(ctx, victim, 0, ""); err != nil {
		t.Fatalf("setup DeleteObjectLocation: %v", err)
	}

	if _, err := s.DeleteObjectLocation(ctx, victim, 1, testDropReason); !errors.Is(err, core.ErrCopyChanged) {
		t.Fatalf("expected ErrCopyChanged when victim already gone, got %v", err)
	}

	locs, _ := s.GetAllObjectLocations(ctx, "bucket/k-vgone")
	if len(locs) != 1 {
		t.Fatalf("expected 1 copy preserved on backend-a, got %d", len(locs))
	}
	if locs[0].BackendName != "backend-a" {
		t.Errorf("expected surviving copy on backend-a, got %s", locs[0].BackendName)
	}
}

// seedStripes replaces a backend's stripe rows with the given per-stripe
// values, so a test controls exactly what the striped total reads.
func seedStripes(t *testing.T, s *Store, backend string, stripes map[int16]int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM backend_quota_stripes WHERE backend_name = ?`, backend); err != nil {
		t.Fatalf("clear stripes: %v", err)
	}
	for stripe, used := range stripes {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO backend_quota_stripes (backend_name, stripe_id, bytes_used) VALUES (?, ?, ?)`,
			backend, stripe, used); err != nil {
			t.Fatalf("seed stripe %d: %v", stripe, err)
		}
	}
}

// rawStripe reads one stripe's value, or zero when the row does not exist.
// Unlike quotaBytesUsed it applies no clamp.
func rawStripe(t *testing.T, s *Store, backend string, stripe int16) int64 {
	t.Helper()
	var used int64
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COALESCE(SUM(bytes_used), 0) FROM backend_quota_stripes WHERE backend_name = ? AND stripe_id = ?`,
		backend, stripe,
	).Scan(&used); err != nil {
		t.Fatalf("read stripe %d: %v", stripe, err)
	}
	return used
}

// rawStripeSum reads a backend's striped total without the clamp, which is
// the figure reconcile makes equal to the ledger.
func rawStripeSum(t *testing.T, s *Store, backend string) int64 {
	t.Helper()
	var used int64
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COALESCE(SUM(bytes_used), 0) FROM backend_quota_stripes WHERE backend_name = ?`, backend,
	).Scan(&used); err != nil {
		t.Fatalf("read stripe total: %v", err)
	}
	return used
}

// mustReconcile runs ReconcileUsage and fails the test on an error.
func mustReconcile(t *testing.T, s *Store) map[string]int64 {
	t.Helper()
	adj, err := s.ReconcileUsage(context.Background())
	if err != nil {
		t.Fatalf("ReconcileUsage: %v", err)
	}
	return adj
}

// TestReconcileUsage_CorrectsDriftAdditively skews a backend's counter across
// several stripes and verifies reconcile adds the difference to stripe zero,
// leaving the other stripes as they were, so the total equals the ledger.
func TestReconcileUsage_CorrectsDriftAdditively(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	mustRecordObject(t, s, "bucket/k1", "backend-a", 100)
	mustRecordObject(t, s, "bucket/k2", "backend-a", 250)
	seedStripes(t, s, "backend-a", map[int16]int64{0: 1000, 3: -200, 7: 450})

	adj := mustReconcile(t, s)

	if want := (map[string]int64{"backend-a": 350 - 1250}); !maps.Equal(adj, want) {
		t.Errorf("adjustments = %v, want %v", adj, want)
	}
	if got := rawStripeSum(t, s, "backend-a"); got != 350 {
		t.Errorf("striped total = %d, want the ledger's 350", got)
	}
	if got := rawStripe(t, s, "backend-a", 0); got != 100 {
		t.Errorf("stripe 0 = %d, want 1000 - 900", got)
	}
	if rawStripe(t, s, "backend-a", 3) != -200 || rawStripe(t, s, "backend-a", 7) != 450 {
		t.Error("stripes other than zero changed, want them left as they were")
	}
}

// TestReconcileUsage_NoDriftReturnsEmpty verifies counters that already agree
// with the ledger are neither reported nor rewritten.
func TestReconcileUsage_NoDriftReturnsEmpty(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustRecordObject(t, s, "bucket/k1", "backend-a", 100)
	mustRecordObject(t, s, "bucket/k2", "backend-b", 40)

	if adj := mustReconcile(t, s); len(adj) != 0 {
		t.Errorf("adjustments = %v, want none", adj)
	}
	if rawStripeSum(t, s, "backend-a") != 100 || rawStripeSum(t, s, "backend-b") != 40 {
		t.Error("an agreeing counter changed")
	}
}

// TestReconcileUsage_BackendWithoutCopiesGoesToZero verifies a backend with no
// ledger rows is brought to zero from either side: a positive total spread over
// stripes and a negative one.
func TestReconcileUsage_BackendWithoutCopiesGoesToZero(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedStripes(t, s, "backend-a", map[int16]int64{2: 300, 9: 200})
	seedStripes(t, s, "backend-b", map[int16]int64{5: -80})

	adj := mustReconcile(t, s)

	if want := (map[string]int64{"backend-a": -500, "backend-b": 80}); !maps.Equal(adj, want) {
		t.Errorf("adjustments = %v, want %v", adj, want)
	}
	for _, backend := range []string{"backend-a", "backend-b"} {
		if got := rawStripeSum(t, s, backend); got != 0 {
			t.Errorf("%s striped total = %d, want 0", backend, got)
		}
	}
}

// TestReconcileUsage_LaterWriteKeepsItsAdjustment verifies a write that
// charges its stripe after a reconcile keeps that charge, so the total still
// equals the ledger and the next reconcile finds nothing to correct.
func TestReconcileUsage_LaterWriteKeepsItsAdjustment(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustRecordObject(t, s, "bucket/k1", "backend-a", 100)
	seedStripes(t, s, "backend-a", map[int16]int64{0: 60, 4: 80})

	if want := (map[string]int64{"backend-a": -40}); !maps.Equal(mustReconcile(t, s), want) {
		t.Fatalf("first reconcile did not apply %v", want)
	}
	mustRecordObject(t, s, "bucket/k2", "backend-a", 200)

	if got := rawStripeSum(t, s, "backend-a"); got != 300 {
		t.Errorf("striped total = %d, want the ledger's 300", got)
	}
	if adj := mustReconcile(t, s); len(adj) != 0 {
		t.Errorf("second reconcile applied %v, want nothing", adj)
	}
}

// TestReconcileUsage_StripeWriteFailureRollsBack verifies a failed correction
// surfaces and leaves every counter as it was.
func TestReconcileUsage_StripeWriteFailureRollsBack(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	seedStripes(t, s, "backend-a", map[int16]int64{0: 70, 6: 30})
	for _, event := range []string{"INSERT", "UPDATE"} {
		if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_stripe_`+event+` BEFORE `+event+
			` ON backend_quota_stripes BEGIN SELECT RAISE(ABORT, 'stripe write rejected'); END`); err != nil {
			t.Fatalf("create %s trigger: %v", event, err)
		}
	}

	if _, err := s.ReconcileUsage(ctx); err == nil {
		t.Fatal("expected the rejected stripe write to surface")
	}
	if got := rawStripeSum(t, s, "backend-a"); got != 100 {
		t.Errorf("striped total = %d, want the untouched 100", got)
	}
}

// TestReconcileUsage_ReadFailureSurfaces verifies a failure reading the drift
// is returned rather than reported as no drift.
func TestReconcileUsage_ReadFailureSurfaces(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.db.ExecContext(context.Background(), `DROP TABLE backend_quota_stripes`); err != nil {
		t.Fatalf("drop stripes: %v", err)
	}
	if adj, err := s.ReconcileUsage(context.Background()); err == nil {
		t.Errorf("ReconcileUsage = %v, nil; want the read error", adj)
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

	queueCleanup(t, s, "backend-a", "bucket/orphan", 512)

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

	// The claim read back from the row settles it.
	retired, err := s.CompleteCleanupItems(ctx, items)
	if err != nil {
		t.Fatalf("CompleteCleanupItems: %v", err)
	}
	if len(retired) != 1 || retired[0] != items[0].ID {
		t.Errorf("retired = %v, want [%d]", retired, items[0].ID)
	}

	depth, _ = s.CleanupQueueDepth(ctx)
	if depth != 0 {
		t.Errorf("depth after complete = %d, want 0", depth)
	}
}

// TestCleanupQueue_Retry verifies a retry with a future backoff takes the
// row out of the pending set until the backoff elapses.
func TestCleanupQueue_Retry(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	queueCleanup(t, s, "backend-a", "bucket/retry", cleanupRowSize)

	items, _ := s.GetPendingCleanups(ctx, 10)
	if held, err := s.RetryCleanupItem(ctx, &items[0], time.Hour, "connection refused"); err != nil || !held {
		t.Fatalf("RetryCleanupItem = %v, %v", held, err)
	}

	// Should not be pending (next_retry is in the future)
	items, _ = s.GetPendingCleanups(ctx, 10)
	if len(items) != 0 {
		t.Errorf("expected 0 pending after retry with future backoff, got %d", len(items))
	}
}

// TestSweepStaleCleanupQueueRows_RemovesMatchAndItsOrphans verifies the
// sweep deletes every cleanup_queue row matching the (path, backend) pair,
// and their bytes stop counting as the backend's orphans.
func TestSweepStaleCleanupQueueRows_RemovesMatchAndItsOrphans(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	// Two queue rows for the same path and backend, plus a row that should
	// be left untouched.
	queueCleanup(t, s, "backend-a", "bucket/k", 100)
	queueCleanup(t, s, "backend-a", "bucket/k", 200)
	queueCleanup(t, s, "backend-a", "bucket/other", 50)

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

	// Only bucket/other's 50 bytes are still queued on backend-a.
	stats, err := s.GetQuotaStats(ctx)
	if err != nil {
		t.Fatalf("GetQuotaStats: %v", err)
	}
	if got := stats["backend-a"].OrphanBytes; got != 50 {
		t.Errorf("orphan_bytes = %d, want 50", got)
	}
}

// TestSweepStaleCleanupQueueRows_NoMatchIsNoOp verifies that calling the
// sweep when no rows match returns 0 and leaves the queued rows' orphans.
func TestSweepStaleCleanupQueueRows_NoMatchIsNoOp(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	queueCleanup(t, s, "backend-a", "bucket/kept", 100)

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

	queueCleanup(t, s, "backend-a", "bucket/k", 100)
	queueCleanup(t, s, "backend-b", "bucket/k", 200)

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

// TestListCopiesByForm_PagesByCursor verifies each page starts after the
// cursor, so a caller hashing as it goes still reaches every copy.
func TestListCopiesByForm_PagesByCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	for _, key := range []string{"bucket/a", "bucket/b", "bucket/c"} {
		mustRecordObject(t, s, key, "backend-a", 10)
	}

	first, err := s.ListCopiesByForm(ctx, &backfillCopies, 2, core.Cursor{})
	if err != nil || len(first) != 2 || first[0].ObjectKey != "bucket/a" || first[1].ObjectKey != "bucket/b" {
		t.Fatalf("first page = %v, %v; want bucket/a and bucket/b", first, err)
	}
	// Hashing the first page takes it out of the set; the cursor still has to
	// land on the one copy left rather than skip it.
	for i := range first {
		if err := s.UpdateCopyMetadata(ctx, &first[i], &core.CopyPatch{ContentHash: "h", Scrubbed: true}); err != nil {
			t.Fatalf("UpdateCopyMetadata: %v", err)
		}
	}
	after := core.Cursor{ObjectKey: first[1].ObjectKey, BackendName: first[1].BackendName}
	rest, err := s.ListCopiesByForm(ctx, &backfillCopies, 2, after)
	if err != nil || len(rest) != 1 || rest[0].ObjectKey != "bucket/c" {
		t.Errorf("second page = %v, %v; want only bucket/c", rest, err)
	}
}

// TestListCopiesByForm_KeyID verifies a key ID narrows the encrypted listing to
// the copies wrapped under it, which is the set a rotation away from it walks.
func TestListCopiesByForm_KeyID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustRecordObject(t, s, "bucket/plain", "backend-a", 10)
	for key, keyID := range map[string]string{"bucket/a": "key-1", "bucket/b": "key-2", "bucket/c": "key-1"} {
		mustRecordEncrypted(t, s, key, "backend-a", keyID, 10, 20)
	}

	page := listByForm(t, s, &core.FormFilter{With: core.StoredEncrypted, KeyID: "key-1"})
	if got := keysOf(page); !slices.Equal(got, []string{"bucket/a", "bucket/c"}) {
		t.Errorf("key-1 copies = %v, want bucket/a and bucket/c", got)
	}
	if got := keysOf(listByForm(t, s, &core.FormFilter{With: core.StoredEncrypted})); len(got) != 3 {
		t.Errorf("every encrypted copy = %v, want three", got)
	}
}

// TestIntegrity_HashOperations verifies the backfill listing drops a copy once
// it is hashed and the scrub listing picks it up.
func TestIntegrity_HashOperations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/b", "backend-a", 200)

	// Both should be without hash
	unhashed := listByForm(t, s, &backfillCopies)
	if len(unhashed) != 2 {
		t.Errorf("expected 2 unhashed, got %d", len(unhashed))
	}

	mustSetContentHash(t, s, "bucket/a", "backend-a", "sha256:abc")

	// Now only 1 without hash
	unhashed = listByForm(t, s, &backfillCopies)
	if len(unhashed) != 1 {
		t.Errorf("expected 1 unhashed, got %d", len(unhashed))
	}

	// GetRandomHashedObjects should return the hashed one
	hashed, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, []string{"backend-a"}, scrubAllCutoff())
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

// TestListDirectoryChildren_UnderscorePrefix verifies a prefix containing an
// underscore still lists its children. The LIKE pattern escapes '_' to '\_',
// and using that escaped length as the child-name substring offset would
// mangle every child.
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

// TestListExpiredObjects_PagesAfterTheCursor verifies After resumes past the
// previous page's last key, which is what lets lifecycle pass over an object
// whose delete failed instead of re-listing it.
func TestListExpiredObjects_PagesAfterTheCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, k := range []string{"bucket/a", "bucket/b", "bucket/c"} {
		mustRecordObject(t, s, k, "backend-a", 10)
	}
	q := core.ExpiredObjectsQuery{Prefix: "bucket/", Cutoff: time.Now().Add(time.Hour), Limit: 2}

	if got := expiredKeys(t, s, q); !slices.Equal(got, []string{"bucket/a", "bucket/b"}) {
		t.Fatalf("first page = %v, want a and b", got)
	}
	q.After = "bucket/b"
	if got := expiredKeys(t, s, q); !slices.Equal(got, []string{"bucket/c"}) {
		t.Errorf("page after bucket/b = %v, want only c", got)
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

// TestEncryptionAdmin_RewriteAndList verifies rewriting objects encrypted and
// back, and listing them by form and key along the way.
func TestEncryptionAdmin_RewriteAndList(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/plain", "backend-a", 1024)

	// List unencrypted
	unenc := listByForm(t, s, &core.FormFilter{Without: core.StoredEncrypted})
	if len(unenc) != 1 {
		t.Errorf("expected 1 unencrypted, got %d", len(unenc))
	}

	// Encrypt in place
	rewriteForm(t, s, "bucket/plain", encryptedForm(1024), 1100)

	// List encrypted
	enc := listByForm(t, s, &core.FormFilter{With: core.StoredEncrypted, KeyID: "key-1"})
	if len(enc) != 1 {
		t.Errorf("expected 1 encrypted, got %d", len(enc))
	}

	// Re-wrap the DEK (rotation), patching the copy the listing returned.
	if err := s.UpdateCopyMetadata(ctx, &enc[0], &core.CopyPatch{EncryptionKey: []byte("new-dek"), KeyID: "key-2"}); err != nil {
		t.Fatalf("UpdateCopyMetadata: %v", err)
	}

	// Old key should have 0 entries
	enc = listByForm(t, s, &core.FormFilter{With: core.StoredEncrypted, KeyID: "key-1"})
	if len(enc) != 0 {
		t.Errorf("expected 0 for old key, got %d", len(enc))
	}

	// Decrypt in place
	rewriteForm(t, s, "bucket/plain", &core.StoredForm{}, 1024)

	locs, _ := s.GetAllObjectLocations(ctx, "bucket/plain")
	if locs[0].Encrypted || locs[0].EncryptionKey != nil || locs[0].SizeBytes != 1024 {
		t.Errorf("row = %+v, want plaintext at 1024 bytes after the decrypt rewrite", locs[0])
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

// TestNotificationQueueDepth verifies the depth counts every notification still
// to be delivered, including one waiting out a backoff.
func TestNotificationQueueDepth(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustInsertNotification(t, s, "test", `{}`, "https://hook.example.com")
	mustInsertNotification(t, s, "test", `{}`, "https://hook.example.com")
	pending, _ := s.GetPendingNotifications(ctx, 10)
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want 2", len(pending))
	}

	if err := s.RetryNotification(ctx, pending[0].ID, time.Hour, "timeout"); err != nil {
		t.Fatalf("RetryNotification: %v", err)
	}
	if n, err := s.NotificationQueueDepth(ctx); err != nil || n != 2 {
		t.Errorf("depth with one backing off = %d, %v; want 2", n, err)
	}
}

// TestNotificationAttempts verifies a notification past ten attempts is
// still due and still counted in the queue depth.
func TestNotificationAttempts(t *testing.T) {
	t.Parallel()
	storetest.CheckNotificationAttempts(t, newTestStore(t))
}

// TestNotificationPendingIndex_UpgradeIndexesEveryRow verifies upgrading a
// database whose pending outbox index skipped rows by attempt count leaves
// an index over every row.
func TestNotificationPendingIndex_UpgradeIndexesEveryRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	rewindToSchemaVersion(t, s, 31)
	if def := pendingOutboxIndexSQL(t, s); !strings.Contains(def, "WHERE") {
		t.Fatalf("rewound index = %q, want the partial index", def)
	}

	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	if def := pendingOutboxIndexSQL(t, s); strings.Contains(def, "WHERE") {
		t.Errorf("upgraded index = %q, want no row filter", def)
	}
}

// pendingOutboxIndexSQL returns the CREATE INDEX statement SQLite stored for
// the pending outbox index.
func pendingOutboxIndexSQL(t *testing.T, s *Store) string {
	t.Helper()
	var def string
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_notification_outbox_pending'`).Scan(&def); err != nil {
		t.Fatalf("read pending outbox index: %v", err)
	}
	return def
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

// TestDeleteBackendData verifies that removing a backend clears its row from
// every table that references backend_quotas and leaves other backends alone.
func TestDeleteBackendData(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.name FROM sqlite_master m, pragma_foreign_key_list(m.name) f
		WHERE m.type = 'table' AND f."table" = 'backend_quotas'`)
	if err != nil {
		t.Fatalf("read foreign keys: %v", err)
	}
	referencing, err := collectRows(rows, "foreign keys", func(r *sql.Rows) (string, error) {
		var name string
		err := r.Scan(&name)
		return name, err
	})
	if err != nil {
		t.Fatalf("read foreign keys: %v", err)
	}

	storetest.CheckBackendRemoval(t, storetest.BackendRemoval{
		Referencing: referencing,
		Exec: func(query, backend string) error {
			_, err := s.db.ExecContext(ctx, query, backend)
			return err
		},
		Count:  func(query, backend string) (int64, error) { return s.countRows(ctx, "rows", query, backend) },
		Remove: func(backend string) error { return s.DeleteBackendData(ctx, backend) },
	}, "backend-a", "backend-b")
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
	mustRecordPart(t, s, &core.RecordPartParams{UploadID: "up-parts", PartNumber: 1, ETag: "etag1", SizeBytes: 100})

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

	_, err = s.ScanMultipartUploads(ctx, core.MultipartUploadFilter{CreatedBefore: time.Now()}, 10, "")
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

	_, err = s.ListObjectsByBackend(ctx, "backend-a", 100, core.SizeCursor{})
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

// columnNames returns the sorted column names of a table or view, so a
// fresh and an upgraded schema can be compared regardless of column order.
func columnNames(t *testing.T, s *Store, table string) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column of %s: %v", table, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	slices.Sort(names)
	return names
}

// TestMigrations_DerivedOrphansAndPartPaths_MatchFreshSchema verifies a
// database upgraded through 0027 and 0028 ends with the columns a fresh
// schema has: no orphan counter on backend_quotas, a part storage key, and a
// backend_capacity view that derives orphan bytes from the cleanup rows.
func TestMigrations_DerivedOrphansAndPartPaths_MatchFreshSchema(t *testing.T) {
	t.Parallel()
	fresh := newTestStore(t)
	upgraded := newTestStore(t)
	rewindToSchemaVersion(t, upgraded, 26)
	if got := columnNames(t, upgraded, "backend_quotas"); !slices.Contains(got, "orphan_bytes") {
		t.Fatalf("rewound backend_quotas = %v, want the orphan counter back", got)
	}
	if err := upgraded.RunMigrations(context.Background()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	for _, table := range []string{"backend_quotas", "multipart_parts", "backend_capacity"} {
		if got, want := columnNames(t, upgraded, table), columnNames(t, fresh, table); !slices.Equal(got, want) {
			t.Errorf("%s columns after upgrade = %v, want the fresh schema's %v", table, got, want)
		}
	}
	queueCleanup(t, upgraded, "backend-a", "bucket/orphan", 70)
	if got := orphanBytes(t, upgraded, "backend-a"); got != 70 {
		t.Errorf("upgraded orphan_bytes = %d, want the queued row's 70", got)
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
	replica := storetest.LocationOn(t, s, "bucket/key1", "backend-b")

	removed, err := s.DeleteObjectLocation(ctx, replica, 0, testDropReason)
	if err != nil {
		t.Fatalf("DeleteObjectLocation: %v", err)
	}
	assertQueuedOnly(t, s, removed, "backend-b", replica.StorageKey, testDropReason, 100)

	locs, _ := s.GetAllObjectLocations(ctx, "bucket/key1")
	if len(locs) != 1 || locs[0].BackendName != "backend-a" {
		t.Errorf("expected only backend-a, got %+v", locs)
	}

	// Reconcile recomputes the counter from the rows that survived, which is
	// what the reported delta has to agree with: the backend that lost its copy
	// holds nothing, and the one that kept it holds the object.
	if _, err := s.ReconcileUsage(ctx); err != nil {
		t.Fatalf("ReconcileUsage: %v", err)
	}
	stats, err := s.GetQuotaStats(ctx)
	if err != nil {
		t.Fatalf("GetQuotaStats: %v", err)
	}
	if stats["backend-b"].BytesUsed != 0 {
		t.Errorf("backend-b bytes_used = %d, want 0 after its copy was removed", stats["backend-b"].BytesUsed)
	}
	if stats["backend-a"].BytesUsed != 100 {
		t.Errorf("backend-a bytes_used = %d, want the 100 bytes it still holds", stats["backend-a"].BytesUsed)
	}

	// Deleting a copy that is already gone reports ErrCopyChanged and makes no
	// spurious debit against the backend that still holds the object.
	if _, err := s.DeleteObjectLocation(ctx, replica, 0, testDropReason); !errors.Is(err, core.ErrCopyChanged) {
		t.Fatalf("DeleteObjectLocation (already gone) = %v, want ErrCopyChanged", err)
	}
	if stats, _ := s.GetQuotaStats(ctx); stats["backend-a"].BytesUsed != 100 {
		t.Errorf("backend-a bytes_used after refused delete = %d, want 100", stats["backend-a"].BytesUsed)
	}
}

// TestDeleteObjectLocation_KeepsOverwrittenCopy verifies a caller holding a
// copy it read before a client overwrote the key on the same backend cannot
// remove the newer write's row: the storage keys differ, so the delete is
// refused and the newer copy and its quota are untouched.
func TestDeleteObjectLocation_KeepsOverwrittenCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	key := "bucket/overwritten"

	record := func(storageKey string) {
		t.Helper()
		req := &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a", StorageKey: storageKey}}, Size: 100}
		if _, _, err := s.RecordObject(ctx, req); err != nil {
			t.Fatalf("RecordObject(%s): %v", storageKey, err)
		}
	}
	record(key + "!old")
	stale := storetest.LocationOn(t, s, key, "backend-a")
	record(key + "!new")

	if _, err := s.DeleteObjectLocation(ctx, stale, 0, testDropReason); !errors.Is(err, core.ErrCopyChanged) {
		t.Fatalf("DeleteObjectLocation(stale) = %v, want ErrCopyChanged", err)
	}
	if got := storetest.LocationOn(t, s, key, "backend-a"); got.StorageKey != key+"!new" {
		t.Errorf("surviving copy at %q, want the newer write at %q", got.StorageKey, key+"!new")
	}
}

// ledgerOf reads the store's per-backend ledger figures, failing the test on
// error.
func ledgerOf(t *testing.T, s *Store) core.LedgerStats {
	t.Helper()
	ledger, err := s.LedgerStats(context.Background())
	if err != nil {
		t.Fatalf("LedgerStats: %v", err)
	}
	return ledger
}

// coverageOf reads verification coverage over the reachable backends as of
// now.
func coverageOf(t *testing.T, s *Store, reachable []string) core.CoverageStat {
	t.Helper()
	return ledgerOf(t, s).Coverage(reachable, time.Now())
}

// TestLedgerStats_ObjectCounts verifies per-backend object count aggregation.
func TestLedgerStats_ObjectCounts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/b", "backend-a", 200)
	mustRecordObject(t, s, "bucket/c", "backend-b", 300)

	counts := ledgerOf(t, s).ObjectCounts()
	if counts["backend-a"] != 2 || counts["backend-b"] != 1 {
		t.Errorf("counts = %v, want backend-a 2 and backend-b 1", counts)
	}
}

// TestLedgerStats_UnhashedCounts pins the per-backend NULL-content_hash count
// that drives the dashboard's "Unverified" column, with one hashed and one
// unhashed object on the same backend.
func TestLedgerStats_UnhashedCounts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	hashed := &core.StoredForm{ContentHash: "deadbeef"}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/b", Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 200, Form: hashed}); err != nil {
		t.Fatalf("RecordObject hashed: %v", err)
	}
	mustRecordObject(t, s, "bucket/c", "backend-b", 300)

	counts := ledgerOf(t, s).UnhashedCounts()
	if counts["backend-a"] != 1 || counts["backend-b"] != 1 {
		t.Errorf("unhashed = %v, want 1 on each backend", counts)
	}
}

// TestScanMultipartUploads verifies each filter narrows the scan, an empty
// filter matches every upload, and the cursor resumes after the last id.
func TestScanMultipartUploads(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateUpload(t, s, "u1", "bucket/a", "backend-a")
	mustCreateUpload(t, s, "u2", "bucket/b", "backend-b")
	mustCreateUpload(t, s, "u3", "bucket/c", "backend-a")

	ids := func(filter core.MultipartUploadFilter, limit int, after string) []string {
		t.Helper()
		uploads, err := s.ScanMultipartUploads(ctx, filter, limit, after)
		if err != nil {
			t.Fatalf("ScanMultipartUploads: %v", err)
		}
		out := make([]string, len(uploads))
		for i := range uploads {
			out[i] = uploads[i].UploadID
		}
		return out
	}

	if got := ids(core.MultipartUploadFilter{}, 10, ""); !slices.Equal(got, []string{"u1", "u2", "u3"}) {
		t.Errorf("unfiltered = %v, want every upload in id order", got)
	}
	if got := ids(core.MultipartUploadFilter{Backend: "backend-a"}, 10, ""); !slices.Equal(got, []string{"u1", "u3"}) {
		t.Errorf("backend-a = %v, want u1 and u3", got)
	}
	if got := ids(core.MultipartUploadFilter{CreatedBefore: time.Now().Add(-24 * time.Hour)}, 10, ""); len(got) != 0 {
		t.Errorf("created a day ago = %v, want none", got)
	}
	if got := ids(core.MultipartUploadFilter{CreatedBefore: time.Now().Add(time.Hour)}, 10, ""); len(got) != 3 {
		t.Errorf("created before an hour from now = %v, want all three", got)
	}
	if got := ids(core.MultipartUploadFilter{}, 1, "u1"); !slices.Equal(got, []string{"u2"}) {
		t.Errorf("page after u1 = %v, want only u2", got)
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

// TestCountReplicationBacklog verifies both directions against one ledger, and
// that an in-flight intent counts toward the under figure as it does in the
// replicator's scan.
func TestCountReplicationBacklog(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/two", "backend-a", 100)
	mustRecordReplica(t, s, "bucket/two", "backend-b", "backend-a", 100)
	mustRecordObject(t, s, "bucket/one", "backend-a", 100)
	mustRecordObject(t, s, "bucket/landing", "backend-a", 100)
	seedPendingIntent(t, s, "intent-landing", "bucket/landing", "backend-b", 100)

	cases := []struct {
		factor int
		want   core.ReplicationBacklog
	}{
		{1, core.ReplicationBacklog{Under: 0, Over: 1}},
		{2, core.ReplicationBacklog{Under: 1, Over: 0}},
		{3, core.ReplicationBacklog{Under: 3, Over: 0}},
	}
	for _, c := range cases {
		got, err := s.CountReplicationBacklog(ctx, c.factor)
		if err != nil {
			t.Fatalf("CountReplicationBacklog(%d): %v", c.factor, err)
		}
		if got != c.want {
			t.Errorf("factor %d: backlog = %+v, want %+v", c.factor, got, c.want)
		}
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
// nothing for nil/empty input without opening a transaction.
func TestDeleteObjectsBatch_EmptyInput(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	got, _, err := s.DeleteObjectsBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("DeleteObjectsBatch(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected nothing queued, got %v", got)
	}
}

// TestDeleteObjectsBatch_RemovesRowsAndDecrementsQuotas verifies a
// mixed batch removes every supplied key's rows, decrements the
// affected backend quotas exactly once each by the summed sizes, queues
// one cleanup row per removed copy, and clears the in-flight intents of
// the removed keys.
func TestDeleteObjectsBatch_RemovesRowsAndDecrementsQuotas(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecordObject(t, s, "bucket/k1", "backend-a", 100)
	mustRecordReplica(t, s, "bucket/k1", "backend-b", "backend-a", 100)
	mustRecordObject(t, s, "bucket/k2", "backend-a", 50)
	seedPendingIntent(t, s, "intent-k2", "bucket/k2", "backend-b", 50)

	got, _, err := s.DeleteObjectsBatch(ctx, []string{"bucket/k1", "bucket/k2", "bucket/missing"})
	if err != nil {
		t.Fatalf("DeleteObjectsBatch: %v", err)
	}
	perReason := map[string]int{}
	for _, c := range got {
		perReason[c.ObjectKey+" "+c.Reason]++
	}
	want := map[string]int{
		"bucket/k1 " + core.CleanupReasonDeleted:          2,
		"bucket/k2 " + core.CleanupReasonDeleted:          1,
		"bucket/k2 " + core.CleanupReasonSupersededIntent: 1,
	}
	if !maps.Equal(perReason, want) {
		t.Errorf("cleanup rows = %v, want %v", perReason, want)
	}
	if depth, _ := s.CleanupQueueDepth(ctx); depth != 4 {
		t.Errorf("queue depth = %d, want 4", depth)
	}
	if n := queryPendingCount(t, s); n != 0 {
		t.Errorf("pending intents = %d, want the deleted key's intent cleared", n)
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

// TestListCopiesByForm_EncryptedRowCarriesPlaintextSize verifies a listing of
// encrypted copies returns the plaintext size the decrypt pass sizes its
// rewrite by.
func TestListCopiesByForm_EncryptedRowCarriesPlaintextSize(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	form := &core.StoredForm{
		Encrypted:     true,
		EncryptionKey: []byte("dek"),
		KeyID:         "key-1",
		PlaintextSize: 1024,
	}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/enc", Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 1100, Form: form}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	locs := listByForm(t, s, &core.FormFilter{With: core.StoredEncrypted})
	if len(locs) != 1 {
		t.Fatalf("expected 1, got %d", len(locs))
	}
	if locs[0].PlaintextSize != 1024 {
		t.Errorf("plaintext_size = %d, want 1024", locs[0].PlaintextSize)
	}
}

// -------------------------------------------------------------------------
// CLEANUP DLQ - END TO END
// -------------------------------------------------------------------------

// TestStore_MoveCleanupToDLQ_GraduatesQueueRow asserts MoveCleanupToDLQ moves
// the queue row into cleanup_dlq with its id, key, size, backend, created_at,
// and the new last_error, and its bytes still count as the backend's orphans.
func TestStore_MoveCleanupToDLQ_GraduatesQueueRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	item := queueCleanup(t, s, "backend-a", "bucket/doomed", cleanupRowSize)
	id := item.ID

	// Stamp an earlier last_error and a distinct created_at to show the move
	// replaces the one and keeps the other.
	createdAt := formatTime(time.Now().Add(-2 * time.Hour))
	if _, err := s.db.ExecContext(ctx,
		`UPDATE cleanup_queue SET last_error = ?, created_at = ? WHERE id = ?`,
		"earlier transient", createdAt, id,
	); err != nil {
		t.Fatalf("stamp row: %v", err)
	}

	moved, err := s.MoveCleanupToDLQ(ctx, &item, "permanent failure")
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
	var firstEnqueued string
	if err := s.db.QueryRowContext(ctx,
		`SELECT first_enqueued_at FROM cleanup_dlq WHERE original_id = ?`, id,
	).Scan(&firstEnqueued); err != nil {
		t.Fatalf("probe first_enqueued_at: %v", err)
	}
	if firstEnqueued != createdAt {
		t.Errorf("first_enqueued_at = %q, want the queue row's created_at %q", firstEnqueued, createdAt)
	}

	// The backend object is still on disk and the DLQ flow is not a
	// write-off, so its bytes still count.
	if orphan := orphanBytes(t, s, "backend-a"); orphan != cleanupRowSize {
		t.Errorf("orphan_bytes=%d, want %d (a dead-lettered row still counts)", orphan, cleanupRowSize)
	}
}

// TestStore_MoveCleanupToDLQ_MissingRowReturnsFalse asserts a no-op
// move on a non-existent id, the outcome when another settle already
// retired the row.
func TestStore_MoveCleanupToDLQ_MissingRowReturnsFalse(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	item := core.CleanupItem{ID: 99999, Claim: &core.CleanupClaim{At: time.Now(), By: testInstanceID}}
	moved, err := s.MoveCleanupToDLQ(context.Background(), &item, "irrelevant")
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
	items := []core.CleanupItem{
		queueCleanup(t, s, "backend-a", "bucket/k1", cleanupRowSize),
		queueCleanup(t, s, "backend-a", "bucket/k2", cleanupRowSize),
	}
	for i := range items {
		if moved, err := s.MoveCleanupToDLQ(ctx, &items[i], ""); err != nil || !moved {
			t.Fatalf("MoveCleanupToDLQ(%d) = %v, %v", items[i].ID, moved, err)
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

	if _, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: "bucket/owned", Backend: "backend-a", Size: 100}); err != nil {
		t.Fatalf("ImportObject(managed): %v", err)
	}
	if _, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: "stray.txt", Backend: "backend-a", Size: 400, Unmanaged: true}); err != nil {
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
	movable, err := s.ListObjectsByBackend(ctx, "backend-a", 10, core.SizeCursor{})
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

	assertNoStray(t, "checksum backfill", listByForm(t, s, &backfillCopies))
}

// TestImportObject_UnreadableEnvelopeIsHiddenFromClientsAndWorkers covers what
// reconcile records for an envelope no key opens. The row keeps its quota, but
// it must not come back into client listings or reads as the key a client
// deleted, and no worker may copy it or try to hash it.
func TestImportObject_UnreadableEnvelopeIsHiddenFromClientsAndWorkers(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/live", "backend-a", 100)
	if _, err := s.ImportObject(ctx, &core.ImportObjectRequest{
		Key: "bucket/gone", Backend: "backend-a", Size: 508, Form: &core.StoredForm{Encrypted: true},
	}); err != nil {
		t.Fatalf("ImportObject: %v", err)
	}

	count, bytes, err := s.BackendObjectStats(ctx, "backend-a")
	if err != nil {
		t.Fatalf("BackendObjectStats: %v", err)
	}
	if count != 2 || bytes != 608 {
		t.Errorf("stats = %d objects / %d bytes, want 2 / 608", count, bytes)
	}

	locs, err := s.GetAllObjectLocations(ctx, "bucket/gone")
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if len(locs) != 1 || !locs[0].Unmanaged {
		t.Fatalf("locations = %+v, want one unmanaged row", locs)
	}

	assertListsOnly(t, s, "bucket/live")
	assertUnreadableOnly(t, s, "bucket/gone", 508)

	under, err := s.GetUnderReplicatedObjects(ctx, 2, 10)
	if err != nil {
		t.Fatalf("GetUnderReplicatedObjects: %v", err)
	}
	assertNotQueued(t, "replication", under, "bucket/gone")
	assertNotQueued(t, "checksum backfill", listByForm(t, s, &backfillCopies), "bucket/gone")
}

// assertUnreadableOnly fails unless the unreadable listing and count report
// exactly the one copy, and a page started past it is empty.
func assertUnreadableOnly(t *testing.T, s *Store, key string, size int64) {
	t.Helper()
	ctx := context.Background()
	unreadable := listByForm(t, s, &unreadableCopies)
	if len(unreadable) != 1 || unreadable[0].ObjectKey != key || unreadable[0].SizeBytes != size {
		t.Fatalf("unreadable = %+v, want only %s", unreadable, key)
	}
	past := core.Cursor{ObjectKey: unreadable[0].ObjectKey, BackendName: unreadable[0].BackendName}
	if rest, err := s.ListCopiesByForm(ctx, &unreadableCopies, 10, past); err != nil || len(rest) != 0 {
		t.Errorf("unreadable listing past the only row = %+v, %v; want none", rest, err)
	}
	if n, err := s.CountCopiesByForm(ctx, unreadableCopies.With, unreadableCopies.Without); err != nil || n != 1 {
		t.Errorf("unreadable count = %d, %v; want 1", n, err)
	}
}

// assertListsOnly fails unless both client listings of bucket/ return exactly
// the one key.
func assertListsOnly(t *testing.T, s *Store, key string) {
	t.Helper()
	ctx := context.Background()
	flat, err := s.ListObjects(ctx, "bucket/", "", 10)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(flat.Objects) != 1 || flat.Objects[0].ObjectKey != key {
		t.Errorf("ListObjects = %+v, want only %s", flat.Objects, key)
	}
	delimited, err := s.ListObjectsDelimited(ctx, "bucket/", "/", "", 10)
	if err != nil {
		t.Fatalf("ListObjectsDelimited: %v", err)
	}
	if len(delimited.Objects) != 1 || delimited.Objects[0].ObjectKey != key {
		t.Errorf("ListObjectsDelimited = %+v, want only %s", delimited.Objects, key)
	}
}

// assertNotQueued fails when a worker candidate scan surfaced key.
func assertNotQueued(t *testing.T, scan string, locs []core.ObjectLocation, key string) {
	t.Helper()
	for i := range locs {
		if locs[i].ObjectKey == key {
			t.Errorf("%s queued for %s", key, scan)
		}
	}
}

// TestRunMigrations_MarksExistingUnreadableRowsUnmanaged covers a database
// whose unreadable rows were imported before import marked them unmanaged.
// The upgrade hides them; a row whose key survives is left alone.
func TestRunMigrations_MarksExistingUnreadableRowsUnmanaged(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.ImportObject(ctx, &core.ImportObjectRequest{
		Key: "bucket/gone", Backend: "backend-a", Size: 508, Form: &core.StoredForm{Encrypted: true},
	}); err != nil {
		t.Fatalf("ImportObject(unreadable): %v", err)
	}
	if _, err := s.ImportObject(ctx, &core.ImportObjectRequest{
		Key: "bucket/kept", Backend: "backend-a", Size: 508,
		Form: &core.StoredForm{Encrypted: true, EncryptionKey: []byte("wrapped"), PlaintextSize: 400},
	}); err != nil {
		t.Fatalf("ImportObject(readable): %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE object_locations SET managed = 1`); err != nil {
		t.Fatalf("mark rows managed: %v", err)
	}
	rewindToSchemaVersion(t, s, 20)

	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	for key, wantUnmanaged := range map[string]bool{"bucket/gone": true, "bucket/kept": false} {
		locs, err := s.GetAllObjectLocations(ctx, key)
		if err != nil {
			t.Fatalf("GetAllObjectLocations(%s): %v", key, err)
		}
		if locs[0].Unmanaged != wantUnmanaged {
			t.Errorf("%s unmanaged = %v, want %v", key, locs[0].Unmanaged, wantUnmanaged)
		}
	}
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
		mustSetContentHash(t, s, key, "backend-a", "sha256:"+key)
	}

	// Stamp two of them, leaving bucket/c never verified.
	mustMarkScrubbed(t, s, "bucket/a", "backend-a")
	time.Sleep(2 * time.Millisecond)
	mustMarkScrubbed(t, s, "bucket/b", "backend-a")

	got, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, []string{"backend-a"}, scrubAllCutoff())
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
		mustSetContentHash(t, s, key, "backend-a", "sha256:"+key)
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

	got, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, []string{"backend-a"}, scrubAllCutoff())
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

// TestLedgerStats_ReportsCoverage verifies the figures the dashboard and the
// alerting rule read: how long the most overdue reachable copy has gone
// unverified, and how many reachable copies have never been verified.
func TestLedgerStats_ReportsCoverage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	reachable := []string{"backend-a"}

	// No hashed copies at all: nothing to report.
	if stat := coverageOf(t, s, reachable); stat != (core.CoverageStat{}) {
		t.Errorf("empty ledger reported %+v, want the zero value", stat)
	}

	for _, key := range []string{"bucket/a", "bucket/b"} {
		mustRecordObject(t, s, key, "backend-a", 100)
		hashAtWrite(t, s, key, "backend-a")
	}

	// Hashed but unverified: both count, and the age reports the backlog from
	// when they were written rather than reading as zero for want of a stamp.
	backdateCreatedAt(t, s, "bucket/a", "backend-a", 48*time.Hour)
	stat := coverageOf(t, s, reachable)
	if stat.NeverVerified != 2 {
		t.Errorf("never verified = %d, want 2", stat.NeverVerified)
	}
	if stat.OldestUnverifiedAge < 47*time.Hour {
		t.Errorf("age = %s, want at least the 48h-old never-verified copy", stat.OldestUnverifiedAge)
	}

	// Verifying the backlogged copy retires it from both figures, so the age
	// falls back to the copy still outstanding.
	mustMarkScrubbed(t, s, "bucket/a", "backend-a")
	stat = coverageOf(t, s, reachable)
	if stat.NeverVerified != 1 {
		t.Errorf("never verified = %d, want 1", stat.NeverVerified)
	}
	if stat.OldestUnverifiedAge >= 47*time.Hour {
		t.Errorf("age = %s, want the stamped copy to have left the head of the queue", stat.OldestUnverifiedAge)
	}
}

// TestLedgerStats_UnreachableBackendIsDeferred pins the property the coverage
// figures exist for: a copy the sweep may not read is reported as deferred
// rather than inflating an age no amount of scrubbing could bring down.
func TestLedgerStats_UnreachableBackendIsDeferred(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	// backend-b stands in for the over-limit backend: the sweep is told it may
	// read backend-a only, which is what affordableBackends produces.
	mustRecordObject(t, s, "bucket/old", "backend-b", 100)
	hashAtWrite(t, s, "bucket/old", "backend-b")
	backdateCreatedAt(t, s, "bucket/old", "backend-b", 720*time.Hour)

	mustRecordObject(t, s, "bucket/new", "backend-a", 100)
	hashAtWrite(t, s, "bucket/new", "backend-a")

	stat := coverageOf(t, s, []string{"backend-a"})
	if stat.Deferred != 1 {
		t.Errorf("deferred = %d, want the copy on the over-limit backend", stat.Deferred)
	}
	if stat.NeverVerified != 1 {
		t.Errorf("never verified = %d, want only the reachable copy", stat.NeverVerified)
	}
	if stat.OldestUnverifiedAge >= 720*time.Hour {
		t.Errorf("age = %s, want the unreachable 30-day copy excluded", stat.OldestUnverifiedAge)
	}
}

// TestHashBackfillPatch_StampsAsVerified pins that the backfill pass does not
// leave behind a copy the very next sweep would re-read. The pass hashed the
// bytes it downloaded, so the copy is verified at that moment.
func TestHashBackfillPatch_StampsAsVerified(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	mustRecordObject(t, s, "bucket/a", "backend-a", 100)
	backdateCreatedAt(t, s, "bucket/a", "backend-a", 48*time.Hour)
	mustSetContentHash(t, s, "bucket/a", "backend-a", "sha256:a")

	stat := coverageOf(t, s, []string{"backend-a"})
	if stat.NeverVerified != 0 {
		t.Errorf("never verified = %d, want the backfilled copy stamped", stat.NeverVerified)
	}
	if stat.OldestUnverifiedAge >= 47*time.Hour {
		t.Errorf("age = %s, want the copy measured from the backfill, not its write", stat.OldestUnverifiedAge)
	}
}

// hashAtWrite gives a copy a content hash without stamping it, which is what
// the write path leaves behind. mustSetContentHash cannot stand in for it: that
// is the backfill pass, and it stamps.
func hashAtWrite(t *testing.T, s *Store, key, backend string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE object_locations SET content_hash = ? WHERE object_key = ? AND backend_name = ?`,
		"sha256:"+key, key, backend,
	); err != nil {
		t.Fatalf("hashing %s/%s at write: %v", key, backend, err)
	}
}

// backdateCreatedAt ages one copy's write timestamp so a test can assert on a
// backlog without waiting for one.
func backdateCreatedAt(t *testing.T, s *Store, key, backend string, by time.Duration) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE object_locations SET created_at = ? WHERE object_key = ? AND backend_name = ?`,
		formatTime(time.Now().Add(-by)), key, backend,
	); err != nil {
		t.Fatalf("backdating created_at for %s/%s: %v", key, backend, err)
	}
}

// TestUpdateCopyMetadata_ReplacedCopyIsChanged verifies a patch based on a copy
// a newer write replaced on the same backend lands on nothing: it reports
// core.ErrCopyChanged and leaves the newer copy as its write left it. A copy
// that is gone altogether reports the same.
func TestUpdateCopyMetadata_ReplacedCopyIsChanged(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k", Size: 10, Copies: []core.ObjectCopy{{Backend: "backend-a", StorageKey: "bucket/k!old"}},
	}); err != nil {
		t.Fatalf("RecordObject(old): %v", err)
	}
	read := *storetest.LocationOn(t, s, "bucket/k", "backend-a")
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k", Size: 10, Copies: []core.ObjectCopy{{Backend: "backend-a", StorageKey: "bucket/k!new"}},
	}); err != nil {
		t.Fatalf("RecordObject(new): %v", err)
	}

	err := s.UpdateCopyMetadata(ctx, &read, &core.CopyPatch{ContentHash: "sha256:old", Scrubbed: true})
	if !errors.Is(err, core.ErrCopyChanged) {
		t.Fatalf("patch of the replaced copy = %v, want ErrCopyChanged", err)
	}
	if got := storetest.LocationOn(t, s, "bucket/k", "backend-a"); got.ContentHash != "" || got.LastScrubbedAt != nil {
		t.Errorf("newer copy patched: hash=%q scrubbed=%v", got.ContentHash, got.LastScrubbedAt)
	}

	gone := &core.ObjectLocation{ObjectKey: "bucket/missing", BackendName: "backend-a", StorageKey: "bucket/missing"}
	if err := s.UpdateCopyMetadata(ctx, gone, &core.CopyPatch{Scrubbed: true}); !errors.Is(err, core.ErrCopyChanged) {
		t.Errorf("patch of an absent copy = %v, want ErrCopyChanged", err)
	}
}

// TestUpdateCopyMetadata_WritesOnlyTheFieldsSet verifies each patch field
// lands on its column and an unset field leaves its column as it was.
func TestUpdateCopyMetadata_WritesOnlyTheFieldsSet(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustRecordObject(t, s, "bucket/k", "backend-a", 10)

	mustPatchCopy(t, s, "bucket/k", "backend-a", &core.CopyPatch{EncryptionKey: []byte("dek"), KeyID: "key-2"})
	mustPatchCopy(t, s, "bucket/k", "backend-a", &core.CopyPatch{Probe: &core.CompressionProbe{Size: 9, Level: "default"}})
	mustSetContentHash(t, s, "bucket/k", "backend-a", "sha256:k")

	got := storetest.LocationOn(t, s, "bucket/k", "backend-a")
	if string(got.EncryptionKey) != "dek" || got.KeyID != "key-2" {
		t.Errorf("key = %q/%q, want dek/key-2 kept through later patches", got.EncryptionKey, got.KeyID)
	}
	if size, level := probeOn(t, s, "bucket/k", "backend-a"); size != 9 || level != "default" {
		t.Errorf("probe = %d/%q, want 9/default kept through later patches", size, level)
	}
	if got.ContentHash != "sha256:k" || got.LastScrubbedAt == nil {
		t.Errorf("hash = %q scrubbed = %v, want sha256:k and a stamp", got.ContentHash, got.LastScrubbedAt)
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

	if _, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, []string{"backend-a"}, scrubAllCutoff()); err == nil {
		t.Error("GetLeastRecentlyScrubbedObjects should surface a closed database")
	}
	// Zeroed ledger figures from a failing database would read as a fully
	// encrypted fleet with nothing deferred, hiding exactly what they report.
	if _, err := s.LedgerStats(ctx); err == nil {
		t.Error("LedgerStats should surface a closed database")
	}
	if _, err := s.CountScrubCandidatesOnBackends(ctx, []string{"backend-a"}, scrubAllCutoff()); err == nil {
		t.Error("CountScrubCandidatesOnBackends should surface a closed database")
	}
	loc := &core.ObjectLocation{ObjectKey: "bucket/a", BackendName: "backend-a", StorageKey: "bucket/a"}
	if err := s.UpdateCopyMetadata(ctx, loc, &core.CopyPatch{Scrubbed: true}); err == nil || errors.Is(err, core.ErrCopyChanged) {
		t.Errorf("UpdateCopyMetadata should surface a closed database, got %v", err)
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
		if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: backend}}, Size: 10}); err != nil {
			t.Fatalf("RecordObject(%s): %v", key, err)
		}
		mustSetContentHash(t, s, key, backend, "sha256:"+key)
	}

	got, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, []string{"backend-a"}, scrubAllCutoff())
	if err != nil {
		t.Fatalf("GetLeastRecentlyScrubbedObjects: %v", err)
	}
	if len(got) != 1 || got[0].BackendName != "backend-a" {
		t.Fatalf("got %d copies %+v, want only the one on backend-a", len(got), got)
	}

	// An empty affordable set means nothing may be read, not everything.
	none, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, nil, scrubAllCutoff())
	if err != nil {
		t.Fatalf("GetLeastRecentlyScrubbedObjects(nil): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("an empty backend list returned %d copies, want 0", len(none))
	}
}

// scrubAllCutoff is a re-verification floor far enough ahead that every row a
// test wrote is due. Tests that exercise the floor itself pass their own.
func scrubAllCutoff() time.Time {
	return time.Now().Add(time.Hour)
}

// TestSqlite_ScrubFloorExcludesRecentlyVerified asserts the re-verification
// floor keeps a freshly written copy out of both the batch and the deferred
// count. A backend holding fewer copies than the batch size would otherwise be
// read in full on every pass, so egress would track the interval rather than
// the staleness of the data.
func TestSqlite_ScrubFloorExcludesRecentlyVerified(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	const key = "bucket/fresh"
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 10}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	mustSetContentHash(t, s, key, "backend-a", "sha256:fresh")

	past := time.Now().Add(-time.Hour)
	got, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, []string{"backend-a"}, past)
	if err != nil {
		t.Fatalf("GetLeastRecentlyScrubbedObjects: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("batch returned %d copies, want 0 behind the floor", len(got))
	}

	n, err := s.CountScrubCandidatesOnBackends(ctx, []string{"backend-a"}, past)
	if err != nil {
		t.Fatalf("CountScrubCandidatesOnBackends: %v", err)
	}
	if n != 0 {
		t.Errorf("deferred count = %d, want 0 behind the floor", n)
	}

	got, err = s.GetLeastRecentlyScrubbedObjects(ctx, 10, []string{"backend-a"}, scrubAllCutoff())
	if err != nil {
		t.Fatalf("GetLeastRecentlyScrubbedObjects(due): %v", err)
	}
	if len(got) != 1 {
		t.Errorf("batch returned %d copies once due, want 1", len(got))
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
		if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-b"}}, Size: 10}); err != nil {
			t.Fatalf("RecordObject(%s): %v", key, err)
		}
		mustSetContentHash(t, s, key, "backend-b", "sha256:"+key)
	}
	// Hashless copies are not scrub candidates and must not be counted.
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/z", Copies: []core.ObjectCopy{{Backend: "backend-b"}}, Size: 10}); err != nil {
		t.Fatalf("RecordObject(z): %v", err)
	}

	n, err := s.CountScrubCandidatesOnBackends(ctx, []string{"backend-b"}, scrubAllCutoff())
	if err != nil {
		t.Fatalf("CountScrubCandidatesOnBackends: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (the hashed copies only)", n)
	}

	if n, err := s.CountScrubCandidatesOnBackends(ctx, nil, scrubAllCutoff()); err != nil || n != 0 {
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
	// comparison put "...00.5Z" after "...00.50000001Z".
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

// TestLedgerStats_PlaintextCopies counts what encrypt-existing would process,
// so the figure an operator sees matches the work the command would actually
// do.
func TestLedgerStats_PlaintextCopies(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	if n := ledgerOf(t, s).PlaintextCopies(); n != 0 {
		t.Fatalf("empty store: count=%d, want 0", n)
	}

	mustRecordObject(t, s, "bucket/plain-a", "backend-a", 100)
	mustRecordObject(t, s, "bucket/plain-b", "backend-a", 100)
	mustRecordEncrypted(t, s, "bucket/secret", "backend-a", "key-0", 100, 132)

	n := ledgerOf(t, s).PlaintextCopies()
	if n != 2 {
		t.Errorf("count = %d, want 2 (the encrypted copy must not be counted)", n)
	}

	// The count and the list agree, so the dashboard figure and the work
	// encrypt-existing performs cannot drift apart.
	listed := listByForm(t, s, &core.FormFilter{Without: core.StoredEncrypted})
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
	mustRecordReplica(t, s, key, "backend-b", "backend-a", 100)
	// Hashed at write rather than by backfill, which stamps: the point here is
	// a copy that carries a hash and has still never been verified.
	for _, backend := range []string{"backend-a", "backend-b"} {
		hashAtWrite(t, s, key, backend)
	}

	// Verify one copy, leaving the other untouched. Per-copy is the point: a
	// replicated object can have one copy checked and another never looked at.
	mustMarkScrubbed(t, s, key, "backend-a")

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

// quotaBytesUsed reads a backend's byte total, summed across its stripes and
// clamped the way every production read of the counter clamps it.
func quotaBytesUsed(t *testing.T, s *Store, backend string) int64 {
	t.Helper()
	var used int64
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COALESCE(MAX(0, SUM(bytes_used)), 0) FROM backend_quota_stripes WHERE backend_name = ?`,
		backend,
	).Scan(&used); err != nil {
		t.Fatalf("read bytes_used: %v", err)
	}
	return used
}

// TestDecryptInPlace_DoesNotDriveQuotaNegative asserts a shrink larger than
// the counter holds clamps at zero instead of underflowing. bytes_used is per
// backend while the delta is per object, so an object whose recorded size
// outruns the counter - a stale size - would otherwise leave a negative
// counter that over-admits every later write until ReconcileUsage runs.
func TestDecryptInPlace_DoesNotDriveQuotaNegative(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	mustRecordEncrypted(t, s, "bucket/secret", "backend-a", "key-1", 100, 900)

	// Drop the counter below what decryption is about to subtract, standing in
	// for a ledger that drifted from the recorded object sizes.
	seedBytesUsed(t, s, "backend-a", 50)

	// 900 -> 100 is a -800 delta against a counter holding 50.
	rewriteForm(t, s, "bucket/secret", &core.StoredForm{}, 100)
	if used := quotaBytesUsed(t, s, "backend-a"); used != 0 {
		t.Errorf("bytes_used = %d, want 0 - a negative counter over-admits later writes", used)
	}
}

// TestEncryptInPlace_DoesNotDriveQuotaNegative asserts the encrypt path
// carries the same clamp. Encryption usually grows a copy, but the delta is
// signed and nothing forbids a placed size smaller than the source's.
func TestEncryptInPlace_DoesNotDriveQuotaNegative(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	mustRecordObject(t, s, "bucket/plain", "backend-a", 900)
	seedBytesUsed(t, s, "backend-a", 50)

	// 900 plaintext -> 100 ciphertext is a -800 delta against a counter of 50.
	rewriteForm(t, s, "bucket/plain", encryptedForm(900), 100)
	if used := quotaBytesUsed(t, s, "backend-a"); used != 0 {
		t.Errorf("bytes_used = %d, want 0", used)
	}
}

// TestEncryptInPlace_ChargesTheCiphertextGrowth pins the ordinary case, so the
// clamp above cannot be mistaken for permission to lose real bytes.
func TestEncryptInPlace_ChargesTheCiphertextGrowth(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	mustRecordObject(t, s, "bucket/plain", "backend-a", 1000)
	before := quotaBytesUsed(t, s, "backend-a")

	rewriteForm(t, s, "bucket/plain", encryptedForm(1000), 1100)
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

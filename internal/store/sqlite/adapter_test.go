// -------------------------------------------------------------------------------
// SQLite TxAdapter - Per-Method Unit Tests
//
// Author: Alex Freidah
//
// Direct coverage for every method on sqliteTxAdapter. The orchestration in
// internal/store/core/ is exercised through the higher-level engine tests in
// store_test.go and pending_test.go; this file pins the adapter shim itself,
// asserting that each translation method behaves correctly against a real
// in-memory SQLite transaction.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// withAdapter opens a transaction on s, hands the wrapping adapter to fn,
// and rolls back when fn returns. Tests that need a committed effect can
// call adapter.tx.Commit() before returning.
func withAdapter(t *testing.T, s *Store, fn func(*sqliteTxAdapter)) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.rawDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	fn(&sqliteTxAdapter{tx: tx})
}

// -------------------------------------------------------------------------
// AcquireKeyLock
// -------------------------------------------------------------------------

// TestAdapter_AcquireKeyLock_NoOp verifies the SQLite adapter no-ops the
// advisory lock - the engine serializes writers, so the call must
// succeed without touching the database.
func TestAdapter_AcquireKeyLock_NoOp(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		if err := a.AcquireKeyLock(context.Background(), "bucket/k"); err != nil {
			t.Errorf("AcquireKeyLock: %v", err)
		}
	})
}

// -------------------------------------------------------------------------
// PENDING TX
// -------------------------------------------------------------------------

// TestAdapter_ClaimPending_TrueWhenInserted verifies the existence probe
// returns true for a pending row that exists.
func TestAdapter_ClaimPending_TrueWhenInserted(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.InsertPending(ctx, &core.PendingObject{
		IntentID: "i-1", ObjectKey: "k", BackendName: "backend-a", SizeBytes: 1,
	}); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		got, err := a.ClaimPending(ctx, "i-1")
		if err != nil {
			t.Fatalf("ClaimPending: %v", err)
		}
		if !got {
			t.Error("expected true for existing intent")
		}
	})
}

// TestAdapter_ClaimPending_FalseWhenMissing verifies the probe returns
// (false, nil) when the row is gone.
func TestAdapter_ClaimPending_FalseWhenMissing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		got, err := a.ClaimPending(context.Background(), "missing")
		if err != nil {
			t.Fatalf("ClaimPending: %v", err)
		}
		if got {
			t.Error("expected false for missing intent")
		}
	})
}

// TestAdapter_InsertPending_NullableFieldsOmitted verifies that empty
// optional fields land as SQL NULL, not the zero value.
func TestAdapter_InsertPending_NullableFieldsOmitted(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		if err := a.InsertPending(ctx, &core.PendingObject{
			IntentID: "i-2", ObjectKey: "k", BackendName: "backend-a", SizeBytes: 5,
		}); err != nil {
			t.Fatalf("InsertPending: %v", err)
		}
		var keyID, plaintext, hash any
		if err := a.tx.QueryRowContext(ctx,
			`SELECT key_id, plaintext_size, content_hash FROM pending_objects WHERE intent_id = ?`,
			"i-2",
		).Scan(&keyID, &plaintext, &hash); err != nil {
			t.Fatalf("query: %v", err)
		}
		if keyID != nil || plaintext != nil || hash != nil {
			t.Errorf("expected SQL NULL for empty optional fields, got %v %v %v", keyID, plaintext, hash)
		}
	})
}

// TestAdapter_DeletePending_RemovesRow verifies the delete removes the
// pending intent row.
func TestAdapter_DeletePending_RemovesRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.InsertPending(ctx, &core.PendingObject{
		IntentID: "i-3", ObjectKey: "k", BackendName: "backend-a", SizeBytes: 1,
	}); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		if err := a.DeletePending(ctx, "i-3"); err != nil {
			t.Fatalf("DeletePending: %v", err)
		}
		got, err := a.ClaimPending(ctx, "i-3")
		if err != nil {
			t.Fatalf("ClaimPending: %v", err)
		}
		if got {
			t.Error("intent still present after DeletePending")
		}
	})
}

// TestAdapter_DeletePendingByBackend_RemovesAllForBackend verifies the
// scoped delete clears every intent for a backend.
func TestAdapter_DeletePendingByBackend_RemovesAllForBackend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	for i, backend := range []string{"backend-a", "backend-a", "backend-b"} {
		if err := s.InsertPending(ctx, &core.PendingObject{
			IntentID: string(rune('a' + i)), ObjectKey: "k", BackendName: backend, SizeBytes: 1,
		}); err != nil {
			t.Fatalf("InsertPending: %v", err)
		}
	}
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		if err := a.DeletePendingByBackend(ctx, "backend-a"); err != nil {
			t.Fatalf("DeletePendingByBackend: %v", err)
		}
		var n int64
		if err := a.tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pending_objects WHERE backend_name = ?`, "backend-a",
		).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("expected 0 backend-a intents after delete, got %d", n)
		}
		// backend-b should be untouched.
		if err := a.tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pending_objects WHERE backend_name = ?`, "backend-b",
		).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Errorf("expected 1 backend-b intent after delete, got %d", n)
		}
	})
}

// -------------------------------------------------------------------------
// OBJECTS TX
// -------------------------------------------------------------------------

// TestAdapter_GetExistingCopiesForUpdate_ReturnsAllCopies verifies the
// adapter returns every copy of a key.
func TestAdapter_GetExistingCopiesForUpdate_ReturnsAllCopies(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecordObject(t, s, "bucket/k", "backend-a", 100)
	mustRecordReplica(t, s, "bucket/k", "backend-b", "backend-a", 100)

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		copies, err := a.GetExistingCopiesForUpdate(ctx, "bucket/k")
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate: %v", err)
		}
		if len(copies) != 2 {
			t.Fatalf("expected 2 copies, got %d", len(copies))
		}
		seen := map[string]bool{}
		for _, ec := range copies {
			seen[ec.BackendName] = true
			if ec.SizeBytes != 100 {
				t.Errorf("size mismatch for %s: %d", ec.BackendName, ec.SizeBytes)
			}
			if ec.CreatedAt.IsZero() {
				t.Errorf("CreatedAt zero for %s", ec.BackendName)
			}
		}
		if !seen["backend-a"] || !seen["backend-b"] {
			t.Errorf("expected copies on both backends, got %v", seen)
		}
	})
}

// TestAdapter_InsertObjectLocation_PreservesEncryptionFields verifies an
// encrypted insert lands every encryption + integrity field.
func TestAdapter_InsertObjectLocation_PreservesEncryptionFields(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		loc := &core.ObjectLocation{
			ObjectKey:     "k",
			BackendName:   "backend-a",
			SizeBytes:     200,
			Encrypted:     true,
			EncryptionKey: []byte("packed"),
			KeyID:         "kid-1",
			PlaintextSize: 180,
			ContentHash:   "abc123",
		}
		if err := a.InsertObjectLocation(ctx, loc); err != nil {
			t.Fatalf("InsertObjectLocation: %v", err)
		}
		var enc int
		var encKey []byte
		var keyID, hash string
		var plaintext int64
		if err := a.tx.QueryRowContext(ctx,
			`SELECT encrypted, encryption_key, key_id, plaintext_size, content_hash
			 FROM object_locations WHERE object_key = ? AND backend_name = ?`, "k", "backend-a",
		).Scan(&enc, &encKey, &keyID, &plaintext, &hash); err != nil {
			t.Fatalf("query: %v", err)
		}
		if enc != 1 || string(encKey) != "packed" || keyID != "kid-1" || plaintext != 180 || hash != "abc123" {
			t.Errorf("encryption fields not preserved: enc=%d key=%q kid=%q plaintext=%d hash=%q",
				enc, string(encKey), keyID, plaintext, hash)
		}
	})
}

// TestAdapter_DeleteObjectCopies_RemovesAllRows verifies every row for
// the key is deleted.
func TestAdapter_DeleteObjectCopies_RemovesAllRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecordObject(t, s, "bucket/k", "backend-a", 100)
	mustRecordReplica(t, s, "bucket/k", "backend-b", "backend-a", 100)

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		if err := a.DeleteObjectCopies(ctx, "bucket/k"); err != nil {
			t.Fatalf("DeleteObjectCopies: %v", err)
		}
		copies, err := a.GetExistingCopiesForUpdate(ctx, "bucket/k")
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate: %v", err)
		}
		if len(copies) != 0 {
			t.Errorf("expected 0 copies after delete, got %d", len(copies))
		}
	})
}

// TestAdapter_CheckObjectExistsOnBackend verifies both true and false
// cases of the existence probe.
func TestAdapter_CheckObjectExistsOnBackend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecordObject(t, s, "bucket/k", "backend-a", 100)

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		got, err := a.CheckObjectExistsOnBackend(ctx, "bucket/k", "backend-a")
		if err != nil {
			t.Fatalf("CheckObjectExistsOnBackend(present): %v", err)
		}
		if !got {
			t.Error("expected true for present (key, backend)")
		}
		got, err = a.CheckObjectExistsOnBackend(ctx, "bucket/k", "backend-b")
		if err != nil {
			t.Fatalf("CheckObjectExistsOnBackend(missing): %v", err)
		}
		if got {
			t.Error("expected false for missing (key, backend)")
		}
	})
}

// TestAdapter_LockObjectOnBackend_ReturnsRow verifies the lock returns
// the row payload when present.
func TestAdapter_LockObjectOnBackend_ReturnsRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	enc := &core.EncryptionMeta{
		Encrypted:     true,
		EncryptionKey: []byte("packed"),
		KeyID:         "kid-1",
		PlaintextSize: 50,
	}
	if _, err := s.RecordObject(ctx, "bucket/k", "backend-a", 75, enc); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		loc, ok, err := a.LockObjectOnBackend(ctx, "bucket/k", "backend-a")
		if err != nil {
			t.Fatalf("LockObjectOnBackend: %v", err)
		}
		if !ok {
			t.Fatal("expected row to be present")
		}
		if loc.SizeBytes != 75 || !loc.Encrypted || loc.KeyID != "kid-1" || loc.PlaintextSize != 50 {
			t.Errorf("row payload mismatch: %+v", loc)
		}
		if string(loc.EncryptionKey) != "packed" {
			t.Errorf("EncryptionKey not preserved: %v", loc.EncryptionKey)
		}
	})
}

// TestAdapter_LockObjectOnBackend_FalseWhenMissing verifies (nil, false,
// nil) is returned when the row is gone.
func TestAdapter_LockObjectOnBackend_FalseWhenMissing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		loc, ok, err := a.LockObjectOnBackend(context.Background(), "bucket/missing", "backend-a")
		if err != nil {
			t.Fatalf("LockObjectOnBackend: %v", err)
		}
		if ok {
			t.Error("expected ok=false for missing row")
		}
		if loc != nil {
			t.Errorf("expected nil loc for missing row, got %+v", loc)
		}
	})
}

// TestAdapter_DeleteObjectFromBackend_RemovesOneRow verifies only the
// targeted (key, backend) row is removed; replicas elsewhere remain.
func TestAdapter_DeleteObjectFromBackend_RemovesOneRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecordObject(t, s, "bucket/k", "backend-a", 100)
	mustRecordReplica(t, s, "bucket/k", "backend-b", "backend-a", 100)

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		if err := a.DeleteObjectFromBackend(ctx, "bucket/k", "backend-a"); err != nil {
			t.Fatalf("DeleteObjectFromBackend: %v", err)
		}
		copies, err := a.GetExistingCopiesForUpdate(ctx, "bucket/k")
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate: %v", err)
		}
		if len(copies) != 1 || copies[0].BackendName != "backend-b" {
			t.Errorf("expected only backend-b after delete, got %+v", copies)
		}
	})
}

// TestAdapter_InsertObjectLocationIfNotExists_InsertsWhenMissing
// verifies the conditional insert returns true when the row is new.
func TestAdapter_InsertObjectLocationIfNotExists_InsertsWhenMissing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		ok, err := a.InsertObjectLocationIfNotExists(ctx, &core.ObjectLocation{
			ObjectKey: "bucket/k", BackendName: "backend-a", SizeBytes: 50,
		})
		if err != nil {
			t.Fatalf("InsertObjectLocationIfNotExists: %v", err)
		}
		if !ok {
			t.Error("expected true for newly inserted row")
		}
	})
}

// TestAdapter_InsertObjectLocationIfNotExists_SkipsWhenPresent verifies
// the conditional insert returns false without erroring when the row
// already exists.
func TestAdapter_InsertObjectLocationIfNotExists_SkipsWhenPresent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecordObject(t, s, "bucket/k", "backend-a", 50)

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		ok, err := a.InsertObjectLocationIfNotExists(ctx, &core.ObjectLocation{
			ObjectKey: "bucket/k", BackendName: "backend-a", SizeBytes: 50,
		})
		if err != nil {
			t.Fatalf("InsertObjectLocationIfNotExists: %v", err)
		}
		if ok {
			t.Error("expected false for existing row")
		}
	})
}

// TestAdapter_InsertReplicaConditional_InsertsWhenSourceExists verifies
// the replica insert succeeds when the source row is present and the
// target row is missing.
func TestAdapter_InsertReplicaConditional_InsertsWhenSourceExists(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecordObject(t, s, "bucket/k", "backend-a", 100)

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		size, ok, err := a.InsertReplicaConditional(ctx, "bucket/k", "backend-b", "backend-a")
		if err != nil {
			t.Fatalf("InsertReplicaConditional: %v", err)
		}
		if !ok {
			t.Error("expected true when source exists and target missing")
		}
		if size != 100 {
			t.Errorf("expected size 100 (source row's), got %d", size)
		}
		// Verify the replica row is there with the source's metadata.
		copies, err := a.GetExistingCopiesForUpdate(ctx, "bucket/k")
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate: %v", err)
		}
		if len(copies) != 2 {
			t.Errorf("expected 2 copies after replica, got %d", len(copies))
		}
	})
}

// TestAdapter_InsertReplicaConditional_FalseWhenSourceMissing verifies
// the replica insert is skipped (without error) when the source row
// has been removed.
func TestAdapter_InsertReplicaConditional_FalseWhenSourceMissing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		size, ok, err := a.InsertReplicaConditional(context.Background(), "bucket/k", "backend-b", "backend-a")
		if err != nil {
			t.Fatalf("InsertReplicaConditional: %v", err)
		}
		if ok {
			t.Error("expected false when source is missing")
		}
		if size != 0 {
			t.Errorf("expected size 0 when source missing, got %d", size)
		}
	})
}

// TestAdapter_InsertReplicaConditional_FalseWhenTargetExists verifies
// the replica insert is skipped (without error) when the target
// already has a copy.
func TestAdapter_InsertReplicaConditional_FalseWhenTargetExists(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecordObject(t, s, "bucket/k", "backend-a", 100)
	mustRecordReplica(t, s, "bucket/k", "backend-b", "backend-a", 100)

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		size, ok, err := a.InsertReplicaConditional(ctx, "bucket/k", "backend-b", "backend-a")
		if err != nil {
			t.Fatalf("InsertReplicaConditional: %v", err)
		}
		if ok {
			t.Error("expected false when target already has a copy")
		}
		if size != 0 {
			t.Errorf("expected size 0 when target already exists, got %d", size)
		}
	})
}

// -------------------------------------------------------------------------
// CLEANUP TX
// -------------------------------------------------------------------------

// TestAdapter_SumAndDeleteCleanupQueueRows_DeletesAndReturnsTotals
// verifies the sum-then-delete returns the row count and total bytes
// of the rows it removed.
func TestAdapter_SumAndDeleteCleanupQueueRows_DeletesAndReturnsTotals(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustEnqueueCleanup(t, s, "backend-a", "bucket/k")
	mustEnqueueCleanup(t, s, "backend-a", "bucket/k") // second row, same key+backend

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		count, total, err := a.SumAndDeleteCleanupQueueRows(ctx, "bucket/k", "backend-a")
		if err != nil {
			t.Fatalf("SumAndDeleteCleanupQueueRows: %v", err)
		}
		if count != 2 || total != 512 {
			t.Errorf("count=%d total=%d, want 2 and 512", count, total)
		}
		// Confirm the rows are gone.
		var n int64
		if err := a.tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM cleanup_queue WHERE object_key = ? AND backend_name = ?`,
			"bucket/k", "backend-a",
		).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("expected 0 rows after sweep, got %d", n)
		}
	})
}

// TestAdapter_SumAndDeleteCleanupQueueRows_NoRowsReturnsZero verifies
// the sum-then-delete returns (0, 0, nil) when no matching rows exist.
func TestAdapter_SumAndDeleteCleanupQueueRows_NoRowsReturnsZero(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		count, total, err := a.SumAndDeleteCleanupQueueRows(context.Background(), "bucket/missing", "backend-a")
		if err != nil {
			t.Fatalf("SumAndDeleteCleanupQueueRows: %v", err)
		}
		if count != 0 || total != 0 {
			t.Errorf("count=%d total=%d, want 0 and 0", count, total)
		}
	})
}

// queueRowExpect is the comparable projection of CleanupQueueRow that
// GetCleanupQueueRow tests assert against. CreatedAt is checked
// separately (it is parsed from the engine's timestamp string and only
// requires non-zero), so it stays out of this struct.
type queueRowExpect struct {
	ID          int64
	BackendName string
	ObjectKey   string
	SizeBytes   int64
	LastError   string
}

// assertQueueRowEquals checks every comparable field of got against
// want and reports a single failure on mismatch, plus a separate
// non-zero check on CreatedAt. Keeps the calling test flat - one
// branch instead of six - so the cognitive-complexity ceiling is not
// breached by per-field assertions. Takes a pointer because
// CleanupQueueRow is heavy (>100 bytes).
func assertQueueRowEquals(t *testing.T, got *core.CleanupQueueRow, want queueRowExpect) {
	t.Helper()
	gotProj := queueRowExpect{
		ID: got.ID, BackendName: got.BackendName, ObjectKey: got.ObjectKey,
		SizeBytes: got.SizeBytes, LastError: got.LastError,
	}
	if gotProj != want {
		t.Errorf("queue row mismatch:\n  got=%+v\n want=%+v", gotProj, want)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt should be parsed from the row, got zero time")
	}
}

// TestAdapter_GetCleanupQueueRow_ReturnsFullRow verifies the SQLite
// adapter projects every queue column - including the parsed CreatedAt
// timestamp and the LastError pointer-deref - back into core.CleanupQueueRow.
// Pinned because MoveCleanupToDLQ relies on this read carrying every
// field the DLQ insert needs.
func TestAdapter_GetCleanupQueueRow_ReturnsFullRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustEnqueueCleanup(t, s, "backend-a", "bucket/k1")
	id := stampCleanupRowError(t, s, "bucket/k1", "boom")

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		row, err := a.GetCleanupQueueRow(ctx, id)
		if err != nil {
			t.Fatalf("GetCleanupQueueRow: %v", err)
		}
		assertQueueRowEquals(t, &row, queueRowExpect{
			ID: id, BackendName: "backend-a", ObjectKey: "bucket/k1",
			SizeBytes: 256, LastError: "boom",
		})
	})
}

// stampCleanupRowError fetches the queue row id for objectKey and
// stamps a non-empty last_error on it. Returns the id so the caller
// can pass it to GetCleanupQueueRow under test. Extracted so the test
// reads as setup-then-assert instead of two raw SQL setup blocks.
func stampCleanupRowError(t *testing.T, s *Store, objectKey, lastError string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM cleanup_queue WHERE object_key = ?`, objectKey,
	).Scan(&id); err != nil {
		t.Fatalf("lookup id for %q: %v", objectKey, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE cleanup_queue SET last_error = ? WHERE id = ?`, lastError, id,
	); err != nil {
		t.Fatalf("set last_error: %v", err)
	}
	return id
}

// TestAdapter_GetCleanupQueueRow_MissingReturnsErrCleanupItemNotFound
// verifies the adapter maps sql.ErrNoRows to the engine-agnostic
// sentinel so MoveCleanupToDLQ can treat a concurrent finaliser race
// as a benign no-op.
func TestAdapter_GetCleanupQueueRow_MissingReturnsErrCleanupItemNotFound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		_, err := a.GetCleanupQueueRow(context.Background(), 99999)
		if !errors.Is(err, core.ErrCleanupItemNotFound) {
			t.Errorf("err=%v, want core.ErrCleanupItemNotFound", err)
		}
	})
}

// TestAdapter_InsertCleanupDLQ_PersistsRow verifies the DLQ insert
// writes every supplied column. The forensic columns (original_id,
// first_enqueued_at, last_error) must round-trip exactly so an
// operator can correlate the DLQ entry back to its origin.
func TestAdapter_InsertCleanupDLQ_PersistsRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	createdAt := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	row := core.CleanupQueueRow{
		ID:          7,
		BackendName: "backend-a",
		ObjectKey:   "bucket/doomed",
		Reason:      "delete_failed",
		SizeBytes:   2048,
		Attempts:    10,
		CreatedAt:   createdAt,
		LastError:   "permanent failure",
	}
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		if err := a.InsertCleanupDLQ(ctx, &row); err != nil {
			t.Fatalf("InsertCleanupDLQ: %v", err)
		}
		// Probe the row back through SQL on the same tx.
		var (
			origID    int64
			backend   string
			key       string
			reason    string
			size      int64
			attempts  int32
			lastError string
		)
		if err := a.tx.QueryRowContext(ctx,
			`SELECT original_id, backend_name, object_key, reason, size_bytes, attempts, COALESCE(last_error, '')
			 FROM cleanup_dlq WHERE original_id = ?`, row.ID,
		).Scan(&origID, &backend, &key, &reason, &size, &attempts, &lastError); err != nil {
			t.Fatalf("probe DLQ row: %v", err)
		}
		if origID != 7 || backend != "backend-a" || key != "bucket/doomed" ||
			reason != "delete_failed" || size != 2048 || attempts != 10 ||
			lastError != "permanent failure" {
			t.Errorf("DLQ row mismatch: orig=%d backend=%q key=%q reason=%q size=%d attempts=%d err=%q",
				origID, backend, key, reason, size, attempts, lastError)
		}
	})
}

// TestAdapter_InsertCleanupDLQ_DefaultsFirstEnqueuedAtWhenZero verifies
// the adapter substitutes "now" when the supplied row carries a zero
// CreatedAt - this guards against producing an invalid timestamp that
// would later confuse operator queries.
func TestAdapter_InsertCleanupDLQ_DefaultsFirstEnqueuedAtWhenZero(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		row := core.CleanupQueueRow{ID: 11, BackendName: "backend-a", ObjectKey: "bucket/k", Reason: "r"}
		if err := a.InsertCleanupDLQ(ctx, &row); err != nil {
			t.Fatalf("InsertCleanupDLQ: %v", err)
		}
		var firstEnqueued string
		if err := a.tx.QueryRowContext(ctx,
			`SELECT first_enqueued_at FROM cleanup_dlq WHERE original_id = ?`, row.ID,
		).Scan(&firstEnqueued); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if firstEnqueued == "" {
			t.Errorf("first_enqueued_at must default to NOW(), got empty string")
		}
	})
}

// TestAdapter_DeleteCleanupItem_RemovesRow verifies the per-id delete
// removes only the targeted row.
func TestAdapter_DeleteCleanupItem_RemovesRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustEnqueueCleanup(t, s, "backend-a", "bucket/k1")
	mustEnqueueCleanup(t, s, "backend-a", "bucket/k2")
	var id1 int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM cleanup_queue WHERE object_key = ?`, "bucket/k1",
	).Scan(&id1); err != nil {
		t.Fatalf("lookup id1: %v", err)
	}
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		if err := a.DeleteCleanupItem(ctx, id1); err != nil {
			t.Fatalf("DeleteCleanupItem: %v", err)
		}
		var n int64
		if err := a.tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM cleanup_queue`,
		).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Errorf("expected 1 remaining row, got %d", n)
		}
	})
}

// -------------------------------------------------------------------------
// QUOTA TX
// -------------------------------------------------------------------------

// TestAdapter_IncrementBackendQuota_AddsBytesUsed verifies the increment
// updates bytes_used.
func TestAdapter_IncrementBackendQuota_AddsBytesUsed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		if err := a.IncrementBackendQuota(ctx, "backend-a", 500); err != nil {
			t.Fatalf("IncrementBackendQuota: %v", err)
		}
		var used int64
		if err := a.tx.QueryRowContext(ctx,
			`SELECT bytes_used FROM backend_quotas WHERE backend_name = ?`, "backend-a",
		).Scan(&used); err != nil {
			t.Fatalf("query: %v", err)
		}
		if used != 500 {
			t.Errorf("bytes_used=%d, want 500", used)
		}
	})
}

// TestAdapter_IncrementBackendQuota_ReturnsErrNoSpaceWhenExceeded
// verifies the guarded UPDATE returns ErrNoSpaceAvailable when the
// quota ceiling would be exceeded.
func TestAdapter_IncrementBackendQuota_ReturnsErrNoSpaceWhenExceeded(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	// Set bytes_limit to a small value via direct SQL on the test store
	// so the increment can exceed it.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE backend_quotas SET bytes_limit = 100 WHERE backend_name = ?`, "backend-a",
	); err != nil {
		t.Fatalf("setup: %v", err)
	}

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		err := a.IncrementBackendQuota(ctx, "backend-a", 200)
		if !errors.Is(err, core.ErrNoSpaceAvailable) {
			t.Errorf("got %v, want ErrNoSpaceAvailable", err)
		}
	})
}

// TestAdapter_DecrementBackendQuota_SubtractsBytesUsed verifies the
// decrement subtracts and clamps at zero.
func TestAdapter_DecrementBackendQuota_SubtractsBytesUsed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustRecordObject(t, s, "bucket/k", "backend-a", 1000)

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		if err := a.DecrementBackendQuota(ctx, "backend-a", 600); err != nil {
			t.Fatalf("DecrementBackendQuota: %v", err)
		}
		var used int64
		if err := a.tx.QueryRowContext(ctx,
			`SELECT bytes_used FROM backend_quotas WHERE backend_name = ?`, "backend-a",
		).Scan(&used); err != nil {
			t.Fatalf("query: %v", err)
		}
		if used != 400 {
			t.Errorf("bytes_used=%d, want 400", used)
		}
	})
}

// TestAdapter_DecrementBackendQuota_ClampsAtZero verifies the decrement
// clamps at zero rather than going negative.
func TestAdapter_DecrementBackendQuota_ClampsAtZero(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		if err := a.DecrementBackendQuota(ctx, "backend-a", 100); err != nil {
			t.Fatalf("DecrementBackendQuota: %v", err)
		}
		var used int64
		if err := a.tx.QueryRowContext(ctx,
			`SELECT bytes_used FROM backend_quotas WHERE backend_name = ?`, "backend-a",
		).Scan(&used); err != nil {
			t.Fatalf("query: %v", err)
		}
		if used != 0 {
			t.Errorf("bytes_used=%d, want 0 (clamped)", used)
		}
	})
}

// TestAdapter_DecrementOrphanBytes_SubtractsAndClamps verifies the
// orphan_bytes decrement subtracts and clamps at zero.
func TestAdapter_DecrementOrphanBytes_SubtractsAndClamps(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.IncrementOrphanBytes(ctx, "backend-a", 1000); err != nil {
		t.Fatalf("IncrementOrphanBytes: %v", err)
	}

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		if err := a.DecrementOrphanBytes(ctx, "backend-a", 600); err != nil {
			t.Fatalf("DecrementOrphanBytes: %v", err)
		}
		var orphans int64
		if err := a.tx.QueryRowContext(ctx,
			`SELECT orphan_bytes FROM backend_quotas WHERE backend_name = ?`, "backend-a",
		).Scan(&orphans); err != nil {
			t.Fatalf("query: %v", err)
		}
		if orphans != 400 {
			t.Errorf("orphan_bytes=%d, want 400", orphans)
		}
		// Over-decrement should clamp at zero.
		if err := a.DecrementOrphanBytes(ctx, "backend-a", 9999); err != nil {
			t.Fatalf("DecrementOrphanBytes(over): %v", err)
		}
		if err := a.tx.QueryRowContext(ctx,
			`SELECT orphan_bytes FROM backend_quotas WHERE backend_name = ?`, "backend-a",
		).Scan(&orphans); err != nil {
			t.Fatalf("query: %v", err)
		}
		if orphans != 0 {
			t.Errorf("orphan_bytes=%d after over-decrement, want 0 (clamped)", orphans)
		}
	})
}

// -------------------------------------------------------------------------
// COMPILE-TIME ASSERTIONS
// -------------------------------------------------------------------------

// TestAdapter_SatisfiesCoreInterfaces is a compile-time check via type
// assertion that *sqliteTxAdapter satisfies every per-feature interface
// embedded in core.TxAdapter. Catches accidental signature drift before
// runtime.
func TestAdapter_SatisfiesCoreInterfaces(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		var _ core.PendingTxAdapter = a
		var _ core.ObjectsTxAdapter = a
		var _ core.CleanupTxAdapter = a
		var _ core.QuotaTxAdapter = a
		var _ core.TxAdapter = a
		_ = time.Now() // silence unused import when other helpers change
	})
}

// TestAdapter_GetExistingCopiesForUpdate_CarriesEncryptionState verifies the
// locked re-read reports each copy's encryption flag and whether its key
// survived. RemoveExcessCopy decides what to delete from these two fields, so
// an adapter that dropped them would silently re-enable destroying the only
// readable copy of a mixed set.
func TestAdapter_GetExistingCopiesForUpdate_CarriesEncryptionState(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	key := "bucket/mixed"

	enc := &core.EncryptionMeta{
		Encrypted:     true,
		EncryptionKey: []byte("wrapped-dek"),
		KeyID:         "key-1",
		PlaintextSize: 1024,
	}
	if _, err := s.RecordObject(ctx, key, "backend-a", 1100, enc); err != nil {
		t.Fatalf("RecordObject encrypted: %v", err)
	}
	if _, _, err := s.RecordReplica(ctx, key, "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		copies, err := a.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate: %v", err)
		}
		if len(copies) != 2 {
			t.Fatalf("expected 2 copies, got %d", len(copies))
		}
		for _, ec := range copies {
			if !ec.Encrypted {
				t.Errorf("%s: Encrypted = false, want true", ec.BackendName)
			}
			if !ec.HasDEK {
				t.Errorf("%s: HasDEK = false, want true (replication copies the key)", ec.BackendName)
			}
		}
	})
}

// TestAdapter_GetExistingCopiesForUpdate_ReportsUnencryptedCopy verifies a
// plain object reports neither flag, so the guard stays inert for copy sets
// that were never encrypted.
func TestAdapter_GetExistingCopiesForUpdate_ReportsUnencryptedCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	key := "bucket/plain"

	if _, err := s.RecordObject(ctx, key, "backend-a", 100, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		copies, err := a.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate: %v", err)
		}
		if len(copies) != 1 {
			t.Fatalf("expected 1 copy, got %d", len(copies))
		}
		if copies[0].Encrypted || copies[0].HasDEK {
			t.Errorf("plain copy reported Encrypted=%v HasDEK=%v, want both false",
				copies[0].Encrypted, copies[0].HasDEK)
		}
	})
}

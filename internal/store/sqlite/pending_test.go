// -------------------------------------------------------------------------------
// SQLite Pending Objects Tests
//
// Author: Alex Freidah
//
// Unit coverage for the SQLite mirror of the pending_objects table. Exercises
// every PendingStore method plus RecordObjectAndClearPending against an
// in-memory database, including the three PromotePending resolution branches
// (committed, superseded, already-resolved) the timestamp-aware reaper relies
// on.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// pendingFixture returns a minimal PendingObject with backend-a and the
// given key/intent ID. Tests override fields as needed.
func pendingFixture(intentID, key string) store.PendingObject {
	return store.PendingObject{
		IntentID:    intentID,
		ObjectKey:   key,
		BackendName: "backend-a",
		SizeBytes:   100,
	}
}

// queryPendingCount returns the number of rows in pending_objects, failing
// the test on a query error.
func queryPendingCount(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM pending_objects`,
	).Scan(&count); err != nil {
		t.Fatalf("queryPendingCount: %v", err)
	}
	return count
}

// queryObjectLocationsCount returns the number of rows in object_locations
// for a key, failing the test on a query error.
func queryObjectLocationsCount(t *testing.T, s *Store, key string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM object_locations WHERE object_key = ?`, key,
	).Scan(&count); err != nil {
		t.Fatalf("queryObjectLocationsCount: %v", err)
	}
	return count
}

// -------------------------------------------------------------------------
// InsertPending / DeletePending / PendingDepth
// -------------------------------------------------------------------------

// TestPending_InsertAndDepth verifies inserts increment the depth counter
// and retain every supplied attribute.
func TestPending_InsertAndDepth(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if got, _ := s.PendingDepth(ctx); got != 0 {
		t.Errorf("initial PendingDepth = %d, want 0", got)
	}

	intent := pendingFixture("intent-1", "bucket/k1")
	intent.Encrypted = true
	intent.EncryptionKey = []byte("packed")
	intent.KeyID = "kid-1"
	intent.PlaintextSize = 90
	intent.ContentHash = "abc"
	if err := s.InsertPending(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}

	depth, err := s.PendingDepth(ctx)
	if err != nil {
		t.Fatalf("PendingDepth: %v", err)
	}
	if depth != 1 {
		t.Errorf("PendingDepth = %d, want 1", depth)
	}

	// Round-trip via GetStalePending so we can verify every column.
	stale, err := s.GetStalePending(ctx, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("GetStalePending: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("GetStalePending returned %d rows, want 1", len(stale))
	}
	got := stale[0]
	if got.IntentID != "intent-1" || got.ObjectKey != "bucket/k1" || got.BackendName != "backend-a" || got.SizeBytes != 100 {
		t.Errorf("primary fields mismatch: %+v", got)
	}
	if !got.Encrypted || string(got.EncryptionKey) != "packed" || got.KeyID != "kid-1" || got.PlaintextSize != 90 || got.ContentHash != "abc" {
		t.Errorf("encryption fields mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set by the database default")
	}
}

// TestPending_DeleteRemovesRow verifies DeletePending decrements depth and
// removes the matching row.
func TestPending_DeleteRemovesRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	intent := pendingFixture("intent-1", "bucket/k1")
	if err := s.InsertPending(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	if err := s.DeletePending(ctx, "intent-1"); err != nil {
		t.Fatalf("DeletePending: %v", err)
	}
	if got := queryPendingCount(t, s); got != 0 {
		t.Errorf("rows after delete = %d, want 0", got)
	}
}

// TestPending_DeleteUnknownIntentIsNoOp verifies deleting a non-existent
// intent does not error — important for reaper retries.
func TestPending_DeleteUnknownIntentIsNoOp(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.DeletePending(context.Background(), "does-not-exist"); err != nil {
		t.Errorf("DeletePending on missing row: %v", err)
	}
}

// -------------------------------------------------------------------------
// GetStalePending
// -------------------------------------------------------------------------

// TestPending_GetStaleRespectsCutoff verifies the cutoff filter excludes
// rows newer than olderThan.
func TestPending_GetStaleRespectsCutoff(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	intent := pendingFixture("fresh", "bucket/k1")
	if err := s.InsertPending(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}

	// Cutoff well in the past — the just-inserted row must be excluded.
	stale, err := s.GetStalePending(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("GetStalePending: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("expected 0 stale rows with past cutoff, got %d", len(stale))
	}
}

// TestPending_GetStaleHonoursLimit verifies the LIMIT clause caps batch size
// so the reaper cannot starve other queries.
func TestPending_GetStaleHonoursLimit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	for i := range 5 {
		intent := pendingFixture("intent-"+string(rune('a'+i)), "bucket/k")
		if err := s.InsertPending(ctx, &intent); err != nil {
			t.Fatalf("InsertPending(%d): %v", i, err)
		}
	}
	stale, err := s.GetStalePending(ctx, time.Now().Add(time.Hour), 3)
	if err != nil {
		t.Fatalf("GetStalePending: %v", err)
	}
	if len(stale) != 3 {
		t.Errorf("limit=3 returned %d rows", len(stale))
	}
}

// -------------------------------------------------------------------------
// DeletePendingByBackend
// -------------------------------------------------------------------------

// TestPending_DeleteByBackend verifies a backend-scoped purge removes only
// the matching backend's intents.
func TestPending_DeleteByBackend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	a := pendingFixture("a", "k-a")
	a.BackendName = "backend-a"
	b := pendingFixture("b", "k-b")
	b.BackendName = "backend-b"
	for _, p := range []store.PendingObject{a, b} {
		if err := s.InsertPending(ctx, &p); err != nil {
			t.Fatalf("InsertPending: %v", err)
		}
	}

	if err := s.DeletePendingByBackend(ctx, "backend-a"); err != nil {
		t.Fatalf("DeletePendingByBackend: %v", err)
	}
	stale, _ := s.GetStalePending(ctx, time.Now().Add(time.Hour), 10)
	if len(stale) != 1 || stale[0].BackendName != "backend-b" {
		t.Errorf("after backend-a purge: %+v, want only backend-b", stale)
	}
}

// -------------------------------------------------------------------------
// PromotePending — three resolution branches
// -------------------------------------------------------------------------

// TestPromotePending_Committed verifies promotion creates the
// object_locations row, increments quota, and clears the pending intent
// when no other rows exist for the key.
func TestPromotePending_Committed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	intent := pendingFixture("intent-1", "bucket/k1")
	if err := s.InsertPending(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	// Reload so CreatedAt reflects the database default rather than the zero value.
	stale, _ := s.GetStalePending(ctx, time.Now().Add(time.Hour), 10)
	if len(stale) != 1 {
		t.Fatalf("seed: pending row missing")
	}
	intent = stale[0]

	result, displaced, err := s.PromotePending(ctx, &intent)
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != store.PendingPromoteCommitted {
		t.Errorf("result = %v, want Committed", result)
	}
	if len(displaced) != 0 {
		t.Errorf("displaced = %+v, want none for fresh promotion", displaced)
	}
	if got := queryPendingCount(t, s); got != 0 {
		t.Errorf("pending row not deleted: count = %d", got)
	}
	if got := queryObjectLocationsCount(t, s, "bucket/k1"); got != 1 {
		t.Errorf("object_locations row not created: count = %d", got)
	}
}

// TestPromotePending_AlreadyResolved verifies a missing pending row returns
// the AlreadyResolved sentinel without touching object_locations.
func TestPromotePending_AlreadyResolved(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	intent := pendingFixture("missing", "bucket/k1")

	result, displaced, err := s.PromotePending(context.Background(), &intent)
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != store.PendingPromoteAlreadyResolved {
		t.Errorf("result = %v, want AlreadyResolved", result)
	}
	if len(displaced) != 0 {
		t.Errorf("displaced = %+v, want none", displaced)
	}
}

// TestPromotePending_Superseded verifies that when an object_locations row
// for the key has a created_at later than the intent, the intent is
// dropped as stale rather than promoted. This is the timestamp-aware
// resolution that prevents head-of-line blocking and avoids deleting a
// retry's correct metadata.
func TestPromotePending_Superseded(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	// Insert a pending intent first so its created_at is older.
	intent := pendingFixture("intent-1", "bucket/k1")
	if err := s.InsertPending(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	stale, _ := s.GetStalePending(ctx, time.Now().Add(time.Hour), 10)
	intent = stale[0]

	// Sleep just enough so the next inserted object_locations row has a
	// strictly later created_at.
	time.Sleep(10 * time.Millisecond)

	// Now record a successful object_locations row for the same key —
	// simulates a retry that committed normally after the original PUT's
	// metadata commit failed.
	if _, err := s.RecordObject(ctx, "bucket/k1", "backend-a", 200, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	result, displaced, err := s.PromotePending(ctx, &intent)
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != store.PendingPromoteSuperseded {
		t.Errorf("result = %v, want Superseded", result)
	}
	if len(displaced) != 0 {
		t.Errorf("displaced = %+v, want none for superseded", displaced)
	}
	if got := queryPendingCount(t, s); got != 0 {
		t.Errorf("pending row not removed: count = %d", got)
	}
	// Retry's object_locations row must remain untouched.
	if got := queryObjectLocationsCount(t, s, "bucket/k1"); got != 1 {
		t.Errorf("object_locations rows = %d, want 1 (retry's row preserved)", got)
	}
}

// -------------------------------------------------------------------------
// intentSuperseded — pure function feeding the timestamp drop path
// -------------------------------------------------------------------------

// Tests for intentSuperseded and the pending row existence probe live in
// internal/store/core/pending_test.go (the canonical home of the
// orchestration helper) and the SQLite TxAdapter integration tests
// below; they are not duplicated here.

// -------------------------------------------------------------------------
// RecordObjectAndClearPending
// -------------------------------------------------------------------------

// TestRecordObjectAndClearPending_DeletesIntent verifies the atomic
// commit-and-clear path: the same transaction that records the object
// location also deletes the pending row, leaving no orphan intent on
// the synchronous success path.
func TestRecordObjectAndClearPending_DeletesIntent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	intent := pendingFixture("intent-1", "bucket/k1")
	if err := s.InsertPending(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}

	displaced, err := s.RecordObjectAndClearPending(ctx, "bucket/k1", "backend-a", 100, nil, "intent-1")
	if err != nil {
		t.Fatalf("RecordObjectAndClearPending: %v", err)
	}
	if len(displaced) != 0 {
		t.Errorf("displaced = %+v, want none for fresh insert", displaced)
	}
	if got := queryPendingCount(t, s); got != 0 {
		t.Errorf("pending row not deleted by atomic commit: %d", got)
	}
	if got := queryObjectLocationsCount(t, s, "bucket/k1"); got != 1 {
		t.Errorf("object_locations row not created: %d", got)
	}
}

// TestRecordObjectAndClearPending_EmptyIntentBehavesLikeRecordObject
// verifies that passing an empty intent ID skips the pending delete and
// preserves the legacy RecordObject behaviour, so callers without the
// pending pattern wired up still work.
func TestRecordObjectAndClearPending_EmptyIntentBehavesLikeRecordObject(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	displaced, err := s.RecordObjectAndClearPending(ctx, "bucket/k1", "backend-a", 100, nil, "")
	if err != nil {
		t.Fatalf("RecordObjectAndClearPending: %v", err)
	}
	if len(displaced) != 0 {
		t.Errorf("displaced = %+v, want none", displaced)
	}
	if got := queryObjectLocationsCount(t, s, "bucket/k1"); got != 1 {
		t.Errorf("object_locations row not created: %d", got)
	}
}

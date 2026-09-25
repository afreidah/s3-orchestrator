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

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// pendingFixture returns a minimal PendingObject with backend-a and the
// given key/intent ID. Tests override fields as needed.
func pendingFixture(intentID, key string) core.PendingObject {
	return core.PendingObject{
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

// queryObjectLocationsOnBackend returns the number of rows in object_locations
// for a key on one backend, failing the test on a query error.
func queryObjectLocationsOnBackend(t *testing.T, s *Store, key, backend string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM object_locations WHERE object_key = ? AND backend_name = ?`, key, backend,
	).Scan(&count); err != nil {
		t.Fatalf("queryObjectLocationsOnBackend: %v", err)
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
	if _, err := s.InsertPendingIfFits(ctx, &intent); err != nil {
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
	if _, err := s.InsertPendingIfFits(ctx, &intent); err != nil {
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
// intent does not error  -  important for reaper retries.
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
	if _, err := s.InsertPendingIfFits(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}

	// Cutoff well in the past  -  the just-inserted row must be excluded.
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
		if _, err := s.InsertPendingIfFits(ctx, &intent); err != nil {
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
	for _, p := range []core.PendingObject{a, b} {
		if _, err := s.InsertPendingIfFits(ctx, &p); err != nil {
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
// PromotePending  -  three resolution branches
// -------------------------------------------------------------------------

// TestPromotePending_Committed verifies promotion creates the
// object_locations row, increments quota, and clears the pending intent
// when no other rows exist for the key.
func TestPromotePending_Committed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	intent := pendingFixture("intent-1", "bucket/k1")
	if _, err := s.InsertPendingIfFits(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	// Reload so CreatedAt reflects the database default rather than the zero value.
	stale, _ := s.GetStalePending(ctx, time.Now().Add(time.Hour), 10)
	if len(stale) != 1 {
		t.Fatalf("seed: pending row missing")
	}
	intent = stale[0]

	result, displaced, _, err := s.PromotePending(ctx, &intent)
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != core.PendingPromoteCommitted {
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

	result, displaced, _, err := s.PromotePending(context.Background(), &intent)
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != core.PendingPromoteAlreadyResolved {
		t.Errorf("result = %v, want AlreadyResolved", result)
	}
	if len(displaced) != 0 {
		t.Errorf("displaced = %+v, want none", displaced)
	}
}

// TestPromotePending_WriteAlreadyClearedTheIntent verifies that a write which
// supersedes an intent removes it as part of its own transaction, so the reaper
// arrives to find nothing left to resolve rather than having to work out that
// the intent is stale.
func TestPromotePending_WriteAlreadyClearedTheIntent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	// Insert a pending intent first so its created_at is older.
	intent := pendingFixture("intent-1", "bucket/k1")
	if _, err := s.InsertPendingIfFits(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	stale, _ := s.GetStalePending(ctx, time.Now().Add(time.Hour), 10)
	intent = stale[0]

	// Sleep just enough so the next inserted object_locations row has a
	// strictly later created_at.
	time.Sleep(10 * time.Millisecond)

	// Now record a successful object_locations row for the same key  -
	// simulates a retry that committed normally after the original PUT's
	// metadata commit failed.
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: "bucket/k1", Size: 200, Copies: []core.ObjectCopy{{Backend: "backend-a"}}}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	result, displaced, _, err := s.PromotePending(ctx, &intent)
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != core.PendingPromoteAlreadyResolved {
		t.Errorf("result = %v, want AlreadyResolved", result)
	}
	if len(displaced) != 0 {
		t.Errorf("displaced = %+v, want none for an intent already cleared", displaced)
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
// intentSuperseded  -  pure function feeding the timestamp drop path
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
	if _, err := s.InsertPendingIfFits(ctx, &intent); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}

	displaced, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k1", Size: 100, Copies: []core.ObjectCopy{{Backend: "backend-a", IntentID: "intent-1"}},
	})
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

	displaced, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k1", Size: 100, Copies: []core.ObjectCopy{{Backend: "backend-a"}},
	})
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

// -------------------------------------------------------------------------
// CommitCompanionCopy  -  two copies racing to one backend
// -------------------------------------------------------------------------

// TestCommitCompanionCopy_DiscardLeavesThePathToTheCopyStillLanding is the
// sequence behind #1527. Two writes to one key each place a further copy on
// backend-b, and a slow backend-b answers them in order: the first write's
// copy resolves before the second write's has landed. The first is discarded,
// rightly, since the second write cleared its intent. Deleting its bytes by
// key and backend at that moment removes whatever is at that path when the
// delete arrives, which is the second copy once it lands, and the second
// copy's commit then records a row for bytes that are gone.
//
// The discard leaves the path to the copy still landing on it instead, and
// leaves that copy's intent as it is: the copy commits normally on top, and
// its intent stays the path's promise of cleanup should it never commit.
func TestCommitCompanionCopy_DiscardLeavesThePathToTheCopyStillLanding(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	first := core.PendingObject{IntentID: "i-first", ObjectKey: "bucket/k", BackendName: "backend-b", SizeBytes: 100, Role: core.PendingRoleCompanion}
	second := core.PendingObject{IntentID: "i-second", ObjectKey: "bucket/k", BackendName: "backend-b", SizeBytes: 100, Role: core.PendingRoleCompanion}
	for _, p := range []*core.PendingObject{&first, &second} {
		if _, err := s.InsertPendingIfFits(ctx, p); err != nil {
			t.Fatalf("InsertPending %s: %v", p.IntentID, err)
		}
	}
	// The second write commits the copy that answered its client, which clears
	// the first write's intent and keeps its own still-running one.
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k", Size: 100,
		Copies:  []core.ObjectCopy{{Backend: "backend-a"}},
		Placing: []core.ObjectCopy{{Backend: "backend-b", IntentID: "i-second"}},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	result, displaced, _, err := s.CommitCompanionCopy(ctx, &first)
	if err != nil {
		t.Fatalf("CommitCompanionCopy first: %v", err)
	}
	if result != core.CompanionCopyUntrusted {
		t.Fatalf("first copy: result = %v, want Untrusted", result)
	}
	if len(displaced) != 0 {
		t.Errorf("first copy's discard reported %+v for deletion while the second copy was still landing at that path", displaced)
	}

	if got := queryPendingCount(t, s); got != 1 {
		t.Errorf("pending rows = %d, want the second copy's intent left in place", got)
	}

	result, displaced, _, err = s.CommitCompanionCopy(ctx, &second)
	if err != nil {
		t.Fatalf("CommitCompanionCopy second: %v", err)
	}
	if result != core.CompanionCopyCommitted {
		t.Errorf("second copy: result = %v, want Committed: its intent was untouched and its bytes are the path's", result)
	}
	if len(displaced) != 0 {
		t.Errorf("second copy's commit reported %+v for deletion, want nothing", displaced)
	}
	if got := queryObjectLocationsOnBackend(t, s, "bucket/k", "backend-b"); got != 1 {
		t.Errorf("object_locations rows on backend-b = %d, want the second copy's", got)
	}
	if got := queryPendingCount(t, s); got != 0 {
		t.Errorf("pending rows = %d, want none once both copies have resolved", got)
	}
}

// TestPromotePending_TwoAbandonedCompanions_TheLastToResolveDeletesThePath
// verifies the reaper's side of the same rule. Two abandoned intents for one
// key and backend: the first resolved leaves the path to the second, whose
// intent is still there for exactly this purpose; the second, finding no
// other intent, hands the bytes back for deletion. Nothing is left behind.
func TestPromotePending_TwoAbandonedCompanions_TheLastToResolveDeletesThePath(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	first := core.PendingObject{IntentID: "i-first", ObjectKey: "bucket/k", BackendName: "backend-b", SizeBytes: 100, Role: core.PendingRoleCompanion}
	second := core.PendingObject{IntentID: "i-second", ObjectKey: "bucket/k", BackendName: "backend-b", SizeBytes: 100, Role: core.PendingRoleCompanion}
	for _, p := range []*core.PendingObject{&first, &second} {
		if _, err := s.InsertPendingIfFits(ctx, p); err != nil {
			t.Fatalf("InsertPending %s: %v", p.IntentID, err)
		}
	}
	// The write that placed them committed its first copy on backend-a and
	// then died; both companions outlived it.
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k", Size: 100,
		Copies:  []core.ObjectCopy{{Backend: "backend-a"}},
		Placing: []core.ObjectCopy{{Backend: "backend-b", IntentID: "i-first"}, {Backend: "backend-b", IntentID: "i-second"}},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	result, displaced, _, err := s.PromotePending(ctx, &first)
	if err != nil {
		t.Fatalf("PromotePending first: %v", err)
	}
	if result != core.PendingPromoteCompanionDiscarded {
		t.Fatalf("first: result = %v, want CompanionDiscarded", result)
	}
	if len(displaced) != 0 {
		t.Errorf("first reported %+v for deletion while the second intent was still live", displaced)
	}

	result, displaced, _, err = s.PromotePending(ctx, &second)
	if err != nil {
		t.Fatalf("PromotePending second: %v", err)
	}
	if result != core.PendingPromoteCompanionDiscarded {
		t.Fatalf("second: result = %v, want CompanionDiscarded", result)
	}
	if len(displaced) != 1 || displaced[0].BackendName != "backend-b" {
		t.Errorf("second reported %+v, want backend-b's bytes now that no intent is left for the path", displaced)
	}
	if got := queryPendingCount(t, s); got != 0 {
		t.Errorf("pending rows = %d, want none", got)
	}
	if got := queryObjectLocationsOnBackend(t, s, "bucket/k", "backend-b"); got != 0 {
		t.Errorf("object_locations rows on backend-b = %d, want none: a companion is never promoted", got)
	}
}

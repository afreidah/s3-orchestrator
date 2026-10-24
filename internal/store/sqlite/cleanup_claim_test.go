// -------------------------------------------------------------------------------
// SQLite Cleanup Queue Claim Pattern Tests
//
// Author: Alex Freidah
//
// Mirrors the Postgres claim-pattern integration tests at the SQLite layer.
// SQLite serialises writes intrinsically so the FOR UPDATE SKIP LOCKED
// concurrency property is replaced by a single sequential check; the rest
// of the contract  -  rows queued already claimed, reclaim after grace,
// settles fenced by the claim, retry clears claim, orphan bytes derived from
// the rows  -  applies identically and is exercised here.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// cleanupRowSize is the size every queued test row carries. cleanupBackend is
// the backend the claim tests queue on. takeoverInstance is the instance that
// reclaims a row whose first claim aged out.
const (
	cleanupRowSize   = int64(256)
	cleanupBackend   = "backend-a"
	takeoverInstance = "instance-takeover"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// queueCleanup queues a cleanup row for key on backend through the tx
// adapter, the way a transaction that stops recording bytes does, and returns
// the item. The row comes back claimed by the store's instance.
func queueCleanup(t *testing.T, s *Store, backend, key string, size int64) core.CleanupItem {
	t.Helper()
	var item core.CleanupItem
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx core.TxAdapter) error {
		var err error
		item, err = tx.InsertCleanup(ctx, &core.CleanupRequest{
			BackendName: backend, ObjectKey: key, StorageKey: key, Reason: "test", SizeBytes: size,
		})
		return err
	}); err != nil {
		t.Fatalf("InsertCleanup(%s): %v", key, err)
	}
	return item
}

// releaseCleanupClaim clears a row's claim, leaving it the way a retry does:
// eligible for any worker.
func releaseCleanupClaim(t *testing.T, s *Store, id int64) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE cleanup_queue SET claimed_at = NULL, claimed_by = NULL WHERE id = ?`, id); err != nil {
		t.Fatalf("release claim %d: %v", id, err)
	}
}

// mustEnqueueCleanup queues an unclaimed cleanup row for key on backend,
// failing the test on error.
func mustEnqueueCleanup(t *testing.T, s *Store, backend, key string) {
	t.Helper()
	releaseCleanupClaim(t, s, queueCleanup(t, s, backend, key, cleanupRowSize).ID)
}

// queuedCleanups returns every cleanup row due now, failing the test on
// error.
func queuedCleanups(t *testing.T, s *Store) []core.CleanupItem {
	t.Helper()
	items, err := s.GetPendingCleanups(context.Background(), 100)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	return items
}

// claimAll claims every due row for instance, whatever its current claim,
// by passing a grace cutoff in the future.
func claimAll(t *testing.T, s *Store, instance string) []core.CleanupItem {
	t.Helper()
	items, err := s.ClaimPendingCleanups(context.Background(), 100, instance, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ClaimPendingCleanups(%s): %v", instance, err)
	}
	return items
}

// takenOver queues one row and lets takeoverInstance reclaim it, returning
// the original holder's item and the new holder's item.
func takenOver(t *testing.T, s *Store) (stale, fresh core.CleanupItem) {
	t.Helper()
	stale = queueCleanup(t, s, cleanupBackend, "k-taken", cleanupRowSize)
	claimed := claimAll(t, s, takeoverInstance)
	if len(claimed) != 1 || claimed[0].ID != stale.ID || !claimed[0].Reclaimed {
		t.Fatalf("takeover claimed %+v, want row %d reclaimed", claimed, stale.ID)
	}
	return stale, claimed[0]
}

// queueRowExists reports whether cleanup_queue still holds row id.
func queueRowExists(t *testing.T, s *Store, id int64) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM cleanup_queue WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count queue row %d: %v", id, err)
	}
	return n > 0
}

// dlqRowExists reports whether cleanup_dlq holds the row graduated from
// queue row id.
func dlqRowExists(t *testing.T, s *Store, id int64) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM cleanup_dlq WHERE original_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count dlq row %d: %v", id, err)
	}
	return n > 0
}

// assertQueuedOnly checks that items, the rows a call returned, are exactly
// one row for path on backend under reason and size bytes, and that the
// queue holds that same row claimed by the store's instance.
func assertQueuedOnly(t *testing.T, s *Store, items []core.CleanupItem, backend, path, reason string, size int64) {
	t.Helper()
	if len(items) != 1 {
		t.Fatalf("cleanup rows = %+v, want one for %s", items, path)
	}
	got := items[0]
	if got.BackendName != backend || got.StorageKey != path || got.Reason != reason || got.SizeBytes != size {
		t.Errorf("cleanup row = %s/%s %q %d bytes, want %s/%s %q %d",
			got.BackendName, got.StorageKey, got.Reason, got.SizeBytes, backend, path, reason, size)
	}
	queued := queuedCleanups(t, s)
	if len(queued) != 1 || queued[0].ID != got.ID {
		t.Fatalf("queue = %+v, want only row %d", queued, got.ID)
	}
	if queued[0].Claim == nil || queued[0].Claim.By != testInstanceID {
		t.Errorf("queued row claim = %+v, want held by %s", queued[0].Claim, testInstanceID)
	}
}

// orphanBytes reads a backend's orphan bytes from backend_capacity, the view
// that derives them from the queued and dead-lettered rows.
func orphanBytes(t *testing.T, s *Store, backendName string) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT orphan_bytes FROM backend_capacity WHERE backend_name = ?`, backendName,
	).Scan(&v); err != nil {
		t.Fatalf("read orphan_bytes: %v", err)
	}
	return v
}

// -------------------------------------------------------------------------
// INSERT CLEANUP
// -------------------------------------------------------------------------

// TestSqlite_InsertCleanup_QueuesClaimedByInstance asserts a queued row
// carries the store's instance id and the claim time the insert returned, and
// stores the path the request names.
func TestSqlite_InsertCleanup_QueuesClaimedByInstance(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	queued := queueCleanup(t, s, cleanupBackend, "k1", cleanupRowSize)

	pending := queuedCleanups(t, s)
	if len(pending) != 1 || pending[0].ID != queued.ID {
		t.Fatalf("pending = %+v, want the one queued row %d", pending, queued.ID)
	}
	got := pending[0]
	if got.Claim == nil {
		t.Fatal("Claim is nil, want the queue time")
	}
	if got.Claim.By != testInstanceID || !got.Claim.At.Equal(queued.Claim.At) {
		t.Errorf("Claim = %+v, want %+v", *got.Claim, *queued.Claim)
	}
	if got.StorageKey != "k1" || got.Reason != "test" || got.SizeBytes != cleanupRowSize {
		t.Errorf("row = %+v, want k1/test/%d", got, cleanupRowSize)
	}
}

// TestSqlite_InsertCleanup_ClaimableOnlyAfterGrace asserts the cleanup
// worker leaves a freshly queued row to the instance that queued it and takes
// it over only once its claim is older than the grace cutoff.
func TestSqlite_InsertCleanup_ClaimableOnlyAfterGrace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	queued := queueCleanup(t, s, cleanupBackend, "k1", cleanupRowSize)

	within, err := s.ClaimPendingCleanups(ctx, 10, "worker", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("claim within grace: %v", err)
	}
	if len(within) != 0 {
		t.Fatalf("claimed %d rows within grace, want 0", len(within))
	}

	after, err := s.ClaimPendingCleanups(ctx, 10, "worker", time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("claim after grace: %v", err)
	}
	if len(after) != 1 || after[0].ID != queued.ID || !after[0].Reclaimed {
		t.Fatalf("claimed = %+v, want row %d reclaimed", after, queued.ID)
	}
}

// TestSqlite_InsertCleanup_NoInstanceLeavesClaimantNull asserts a store
// opened without an instance id queues rows with no claimant, and that the
// returned empty claimant still settles the row.
func TestSqlite_InsertCleanup_NoInstanceLeavesClaimantNull(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	a := &sqliteTxAdapter{q: s.db}
	item, err := a.InsertCleanup(ctx, &core.CleanupRequest{
		BackendName: cleanupBackend, ObjectKey: "k1", StorageKey: "k1", Reason: "test", SizeBytes: 1,
	})
	if err != nil {
		t.Fatalf("InsertCleanup: %v", err)
	}
	if item.Claim == nil || item.Claim.By != "" {
		t.Fatalf("Claim = %+v, want one with an empty claimant", item.Claim)
	}
	var claimedBy *string
	if err := s.db.QueryRowContext(ctx,
		`SELECT claimed_by FROM cleanup_queue WHERE id = ?`, item.ID).Scan(&claimedBy); err != nil {
		t.Fatalf("read claimed_by: %v", err)
	}
	if claimedBy != nil {
		t.Errorf("claimed_by = %q, want NULL", *claimedBy)
	}

	retired, err := s.CompleteCleanupItems(ctx, []core.CleanupItem{item})
	if err != nil {
		t.Fatalf("CompleteCleanupItems: %v", err)
	}
	if len(retired) != 1 || retired[0] != item.ID {
		t.Errorf("retired = %v, want [%d]", retired, item.ID)
	}
}

// -------------------------------------------------------------------------
// CLAIM
// -------------------------------------------------------------------------

// TestSqlite_ClaimPendingCleanups_StampsClaim asserts a claim of an
// unclaimed row returns it with Reclaimed=false and the claim it stamped, and
// that a follow-up GetPendingCleanups reads the same claim back.
func TestSqlite_ClaimPendingCleanups_StampsClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustEnqueueCleanup(t, s, cleanupBackend, "k1")

	before := time.Now()
	claimed, err := s.ClaimPendingCleanups(ctx, 10, "instance-X", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ClaimPendingCleanups: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed len = %d, want 1", len(claimed))
	}
	if claimed[0].Reclaimed {
		t.Errorf("fresh claim Reclaimed=true, want false")
	}
	claim := claimed[0].Claim
	if claim == nil || claim.By != "instance-X" || claim.At.Before(before) || time.Since(claim.At) > time.Minute {
		t.Fatalf("returned claim = %+v, want instance-X stamped now", claim)
	}

	pending := queuedCleanups(t, s)
	if len(pending) != 1 {
		t.Fatalf("pending len = %d, want 1", len(pending))
	}
	if pending[0].Claim == nil || pending[0].Claim.By != "instance-X" || !pending[0].Claim.At.Equal(claim.At) {
		t.Errorf("stored claim = %+v, want %+v", pending[0].Claim, *claim)
	}
	if pending[0].Reclaimed {
		t.Error("GetPendingCleanups set Reclaimed, want it left false")
	}
}

// TestSqlite_GetPendingCleanups_UnclaimedHasNilClaim asserts a row no
// instance holds reads back with no claim.
func TestSqlite_GetPendingCleanups_UnclaimedHasNilClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustEnqueueCleanup(t, s, cleanupBackend, "k1")

	pending := queuedCleanups(t, s)
	if len(pending) != 1 {
		t.Fatalf("pending len = %d, want 1", len(pending))
	}
	if pending[0].Claim != nil {
		t.Errorf("Claim = %+v, want nil", *pending[0].Claim)
	}
}

// TestSqlite_ClaimPendingCleanups_ReclaimAfterGrace asserts a stale claim
// is reclaimable and the returned item carries Reclaimed=true.
func TestSqlite_ClaimPendingCleanups_ReclaimAfterGrace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	id := queueCleanup(t, s, cleanupBackend, "k1", cleanupRowSize).ID

	// Backdate the claim so the next claim sees it as stale.
	stale := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE cleanup_queue SET claimed_at = ? WHERE id = ?`, stale, id,
	); err != nil {
		t.Fatalf("backdate claim: %v", err)
	}

	second, err := s.ClaimPendingCleanups(ctx, 10, "B", time.Now().Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("second claim len = %d, want 1", len(second))
	}
	if !second[0].Reclaimed {
		t.Errorf("Reclaimed=false on stale-claim recovery, want true")
	}

	// Stricter cutoff (15m) must NOT reclaim a 10m-old claim.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE cleanup_queue SET claimed_at = ? WHERE id = ?`, stale, id,
	); err != nil {
		t.Fatalf("re-backdate: %v", err)
	}
	third, err := s.ClaimPendingCleanups(ctx, 10, "C", time.Now().Add(-15*time.Minute))
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if len(third) != 0 {
		t.Errorf("strict cutoff reclaimed a 10m-old claim; got %d rows, want 0", len(third))
	}
}

// TestSqlite_Cleanup_MaxAttemptsExcluded asserts a row at the attempt ceiling
// is left out of the pending snapshot, the claim, and the queue depth, while
// a row one attempt below it is included.
func TestSqlite_Cleanup_MaxAttemptsExcluded(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	below := queueCleanup(t, s, cleanupBackend, "k-below", cleanupRowSize)
	atCeiling := queueCleanup(t, s, cleanupBackend, "k-ceiling", cleanupRowSize)
	setAttempts(t, s, below.ID, core.MaxCleanupAttempts-1)
	setAttempts(t, s, atCeiling.ID, core.MaxCleanupAttempts)

	pending := queuedCleanups(t, s)
	if len(pending) != 1 || pending[0].ID != below.ID {
		t.Errorf("pending = %+v, want only row %d", pending, below.ID)
	}
	if depth, err := s.CleanupQueueDepth(ctx); err != nil || depth != 1 {
		t.Errorf("CleanupQueueDepth = %d, %v; want 1", depth, err)
	}
	claimed := claimAll(t, s, takeoverInstance)
	if len(claimed) != 1 || claimed[0].ID != below.ID {
		t.Errorf("claimed = %+v, want only row %d", claimed, below.ID)
	}
}

// setAttempts sets a queue row's attempt counter.
func setAttempts(t *testing.T, s *Store, id int64, attempts int) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE cleanup_queue SET attempts = ? WHERE id = ?`, attempts, id); err != nil {
		t.Fatalf("set attempts on %d: %v", id, err)
	}
}

// -------------------------------------------------------------------------
// COMPLETE
// -------------------------------------------------------------------------

// TestSqlite_CompleteCleanupItems_RetiresRowAndOrphans asserts completing a
// row removes it and its bytes from the backend's orphans and returns its id,
// and a re-complete of the missing row retires nothing.
func TestSqlite_CompleteCleanupItems_RetiresRowAndOrphans(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	const size = int64(4096)
	item := queueCleanup(t, s, cleanupBackend, "k1", size)
	if got := orphanBytes(t, s, cleanupBackend); got != size {
		t.Fatalf("orphan_bytes = %d with the row queued, want %d", got, size)
	}

	retired, err := s.CompleteCleanupItems(ctx, []core.CleanupItem{item})
	if err != nil {
		t.Fatalf("CompleteCleanupItems: %v", err)
	}
	if len(retired) != 1 || retired[0] != item.ID {
		t.Errorf("retired = %v, want [%d]", retired, item.ID)
	}
	if got := orphanBytes(t, s, cleanupBackend); got != 0 {
		t.Errorf("orphan_bytes = %d after complete, want 0", got)
	}
	if depth, _ := s.CleanupQueueDepth(ctx); depth != 0 {
		t.Errorf("queue depth = %d after complete, want 0", depth)
	}

	retired, err = s.CompleteCleanupItems(ctx, []core.CleanupItem{item})
	if err != nil {
		t.Fatalf("CompleteCleanupItems (repeat): %v", err)
	}
	if len(retired) != 0 {
		t.Errorf("repeat retired = %v, want none", retired)
	}
}

// TestSqlite_CompleteCleanupItems_EmptyIsNoOp asserts an empty batch returns
// no ids and no error.
func TestSqlite_CompleteCleanupItems_EmptyIsNoOp(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	retired, err := s.CompleteCleanupItems(context.Background(), nil)
	if err != nil || retired != nil {
		t.Errorf("CompleteCleanupItems(nil) = %v, %v; want nil, nil", retired, err)
	}
}

// TestSqlite_CompleteCleanupItems_RetiresOnlyHeldRows asserts a batch mixing
// a held row with one whose claim was taken over retires only the held row.
func TestSqlite_CompleteCleanupItems_RetiresOnlyHeldRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	stale, fresh := takenOver(t, s)
	held := queueCleanup(t, s, cleanupBackend, "k-held", cleanupRowSize)

	retired, err := s.CompleteCleanupItems(context.Background(), []core.CleanupItem{stale, held})
	if err != nil {
		t.Fatalf("CompleteCleanupItems: %v", err)
	}
	if len(retired) != 1 || retired[0] != held.ID {
		t.Errorf("retired = %v, want [%d]", retired, held.ID)
	}
	if !queueRowExists(t, s, fresh.ID) {
		t.Error("taken-over row was retired by its stale holder")
	}
}

// TestSqlite_CompleteCleanupItems_LostClaimLeavesRow asserts the original
// holder of a taken-over row retires nothing, and the new holder retires it.
func TestSqlite_CompleteCleanupItems_LostClaimLeavesRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	stale, fresh := takenOver(t, s)

	retired, err := s.CompleteCleanupItems(ctx, []core.CleanupItem{stale})
	if err != nil {
		t.Fatalf("CompleteCleanupItems(stale): %v", err)
	}
	if len(retired) != 0 || !queueRowExists(t, s, stale.ID) {
		t.Fatalf("stale holder retired %v, want nothing", retired)
	}

	retired, err = s.CompleteCleanupItems(ctx, []core.CleanupItem{fresh})
	if err != nil {
		t.Fatalf("CompleteCleanupItems(fresh): %v", err)
	}
	if len(retired) != 1 || queueRowExists(t, s, fresh.ID) {
		t.Errorf("new holder retired %v, want [%d]", retired, fresh.ID)
	}
}

// TestSqlite_CompleteCleanupItems_DeleteErrorFails asserts a failing delete
// inside the batch transaction is returned with no ids.
func TestSqlite_CompleteCleanupItems_DeleteErrorFails(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	item := queueCleanup(t, s, cleanupBackend, "k1", cleanupRowSize)
	if _, err := s.db.ExecContext(context.Background(),
		`CREATE TRIGGER block_cleanup_delete BEFORE DELETE ON cleanup_queue
		 BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	retired, err := s.CompleteCleanupItems(context.Background(), []core.CleanupItem{item})
	if err == nil || retired != nil {
		t.Errorf("CompleteCleanupItems = %v, %v; want nil and an error", retired, err)
	}
}

// TestSqlite_ClaimPendingCleanups_StampErrorFails asserts a failing claim
// stamp is returned with no items.
func TestSqlite_ClaimPendingCleanups_StampErrorFails(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustEnqueueCleanup(t, s, cleanupBackend, "k1")
	if _, err := s.db.ExecContext(context.Background(),
		`CREATE TRIGGER block_cleanup_update BEFORE UPDATE ON cleanup_queue
		 BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	items, err := s.ClaimPendingCleanups(context.Background(), 10, takeoverInstance, time.Now())
	if err == nil || items != nil {
		t.Errorf("ClaimPendingCleanups = %v, %v; want nil and an error", items, err)
	}
}

// TestSqlite_GetPendingCleanups_ScanErrorFails asserts a row whose column
// cannot be read into the item fails the read.
func TestSqlite_GetPendingCleanups_ScanErrorFails(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	item := queueCleanup(t, s, cleanupBackend, "k1", cleanupRowSize)
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE cleanup_queue SET size_bytes = 'not-a-number' WHERE id = ?`, item.ID); err != nil {
		t.Fatalf("corrupt size_bytes: %v", err)
	}
	if _, err := s.GetPendingCleanups(context.Background(), 10); err == nil {
		t.Error("expected a scan error")
	}
}

// -------------------------------------------------------------------------
// RETRY
// -------------------------------------------------------------------------

// TestSqlite_RetryCleanupItem_ClearsClaim asserts that a retry reports the
// row was held, bumps attempts, records the error, and clears the claim so
// the next worker tick can re-claim immediately.
func TestSqlite_RetryCleanupItem_ClearsClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	item := queueCleanup(t, s, cleanupBackend, "k1", cleanupRowSize)

	// A backoff already in the past makes the row unambiguously due, so the
	// assertion below is about the claim being cleared rather than about
	// whether the clock advanced past a sub-millisecond retry stamp.
	held, err := s.RetryCleanupItem(ctx, &item, -time.Second, "transient")
	if err != nil || !held {
		t.Fatalf("RetryCleanupItem = %v, %v; want true, nil", held, err)
	}

	pending := queuedCleanups(t, s)
	if len(pending) != 1 {
		t.Fatalf("pending len = %d, want 1", len(pending))
	}
	if pending[0].Claim != nil {
		t.Errorf("Claim = %+v after retry, want nil", *pending[0].Claim)
	}
	if pending[0].Attempts != 1 {
		t.Errorf("attempts = %d after retry, want 1", pending[0].Attempts)
	}
	var lastError string
	if err := s.db.QueryRowContext(ctx,
		`SELECT last_error FROM cleanup_queue WHERE id = ?`, item.ID).Scan(&lastError); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	if lastError != "transient" {
		t.Errorf("last_error = %q, want transient", lastError)
	}
}

// TestSqlite_RetryCleanupItem_LostClaimIsFenced asserts the original holder
// of a taken-over row cannot reschedule it, and the new holder can.
func TestSqlite_RetryCleanupItem_LostClaimIsFenced(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	stale, fresh := takenOver(t, s)

	held, err := s.RetryCleanupItem(ctx, &stale, time.Hour, "stale")
	if err != nil || held {
		t.Fatalf("stale RetryCleanupItem = %v, %v; want false, nil", held, err)
	}
	pending := queuedCleanups(t, s)
	if len(pending) != 1 || pending[0].Attempts != 0 || pending[0].Claim == nil || pending[0].Claim.By != takeoverInstance {
		t.Fatalf("row after stale retry = %+v, want untouched and held by %s", pending, takeoverInstance)
	}

	held, err = s.RetryCleanupItem(ctx, &fresh, time.Hour, "fresh")
	if err != nil || !held {
		t.Errorf("fresh RetryCleanupItem = %v, %v; want true, nil", held, err)
	}
}

// TestSqlite_Cleanup_NilClaimMatchesNothing asserts an item with no claim
// settles no row through complete, retry, or dead-letter.
func TestSqlite_Cleanup_NilClaimMatchesNothing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustEnqueueCleanup(t, s, cleanupBackend, "k1")
	item := queuedCleanups(t, s)[0]

	if retired, err := s.CompleteCleanupItems(ctx, []core.CleanupItem{item}); err != nil || len(retired) != 0 {
		t.Errorf("CompleteCleanupItems = %v, %v; want none", retired, err)
	}
	if held, err := s.RetryCleanupItem(ctx, &item, time.Hour, "x"); err != nil || held {
		t.Errorf("RetryCleanupItem = %v, %v; want false", held, err)
	}
	if moved, err := s.MoveCleanupToDLQ(ctx, &item, "x"); err != nil || moved {
		t.Errorf("MoveCleanupToDLQ = %v, %v; want false", moved, err)
	}
	if !queueRowExists(t, s, item.ID) {
		t.Error("unclaimed row was settled")
	}
}

// TestSqlite_SweepStaleCleanupQueueRows_ClosedStoreErrors verifies the
// sweep's delete error is returned rather than reported as zero rows.
func TestSqlite_SweepStaleCleanupQueueRows_ClosedStoreErrors(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	s.Close()
	if _, err := s.SweepStaleCleanupQueueRows(context.Background(), "k", cleanupBackend); err == nil {
		t.Error("expected an error from a closed store")
	}
}

// TestSqlite_Cleanup_ClosedStoreErrors verifies every queue read and settle
// returns its error from a closed store.
func TestSqlite_Cleanup_ClosedStoreErrors(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	item := queueCleanup(t, s, cleanupBackend, "k1", cleanupRowSize)
	s.Close()
	ctx := context.Background()

	if _, err := s.GetPendingCleanups(ctx, 10); err == nil {
		t.Error("GetPendingCleanups: expected an error")
	}
	if _, err := s.ClaimPendingCleanups(ctx, 10, takeoverInstance, time.Now()); err == nil {
		t.Error("ClaimPendingCleanups: expected an error")
	}
	if _, err := s.CompleteCleanupItems(ctx, []core.CleanupItem{item}); err == nil {
		t.Error("CompleteCleanupItems: expected an error")
	}
	if _, err := s.RetryCleanupItem(ctx, &item, time.Second, "x"); err == nil {
		t.Error("RetryCleanupItem: expected an error")
	}
	if _, err := s.MoveCleanupToDLQ(ctx, &item, "x"); err == nil {
		t.Error("MoveCleanupToDLQ: expected an error")
	}
	if _, err := s.RequeueCleanupDLQ(ctx, ""); err == nil {
		t.Error("RequeueCleanupDLQ: expected an error")
	}
	if _, err := s.CleanupQueueDepth(ctx); err == nil {
		t.Error("CleanupQueueDepth: expected an error")
	}
}

// -------------------------------------------------------------------------
// DEAD-LETTER MOVES
// -------------------------------------------------------------------------

// TestSqlite_MoveCleanupToDLQ_LostClaimIsFenced asserts the original holder
// of a taken-over row cannot dead-letter it, and the new holder can.
func TestSqlite_MoveCleanupToDLQ_LostClaimIsFenced(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	stale, fresh := takenOver(t, s)

	moved, err := s.MoveCleanupToDLQ(ctx, &stale, "stale")
	if err != nil || moved {
		t.Fatalf("stale MoveCleanupToDLQ = %v, %v; want false, nil", moved, err)
	}
	if !queueRowExists(t, s, stale.ID) || dlqRowExists(t, s, stale.ID) {
		t.Fatal("stale holder moved the row")
	}

	moved, err = s.MoveCleanupToDLQ(ctx, &fresh, "fresh")
	if err != nil || !moved {
		t.Fatalf("fresh MoveCleanupToDLQ = %v, %v; want true, nil", moved, err)
	}
	if queueRowExists(t, s, fresh.ID) || !dlqRowExists(t, s, fresh.ID) {
		t.Error("new holder did not move the row")
	}
}

// TestSqlite_MoveRows_NothingCopiedSkipsDelete asserts a move whose insert
// copies no rows reports zero and never runs its delete.
func TestSqlite_MoveRows_NothingCopiedSkipsDelete(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	item := queueCleanup(t, s, cleanupBackend, "k1", cleanupRowSize)

	n, err := s.moveRows(context.Background(), "test move",
		`INSERT INTO cleanup_dlq (original_id, backend_name, object_key, storage_key, reason, size_bytes, attempts, first_enqueued_at)
		 SELECT id, backend_name, object_key, storage_key, reason, size_bytes, attempts, created_at
		 FROM cleanup_queue WHERE id = ?`, []any{item.ID + 1000},
		`DELETE FROM cleanup_queue`, nil,
	)
	if err != nil || n != 0 {
		t.Fatalf("moveRows = %d, %v; want 0, nil", n, err)
	}
	if !queueRowExists(t, s, item.ID) {
		t.Error("delete ran although nothing was copied")
	}
}

// TestSqlite_MoveRows_ErrorsRollBack asserts a failing insert or delete
// returns a labelled error and leaves both tables as they were.
func TestSqlite_MoveRows_ErrorsRollBack(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	item := queueCleanup(t, s, cleanupBackend, "k1", cleanupRowSize)
	copyAll := `INSERT INTO cleanup_dlq (original_id, backend_name, object_key, storage_key, reason, size_bytes, attempts, first_enqueued_at)
		 SELECT id, backend_name, object_key, storage_key, reason, size_bytes, attempts, created_at FROM cleanup_queue`

	if _, err := s.moveRows(ctx, "bad insert", `INSERT INTO no_such_table VALUES (1)`, nil, `DELETE FROM cleanup_queue`, nil); err == nil {
		t.Error("bad insert: expected an error")
	}
	if _, err := s.moveRows(ctx, "bad delete", copyAll, nil, `DELETE FROM no_such_table`, nil); err == nil {
		t.Error("bad delete: expected an error")
	}
	if !queueRowExists(t, s, item.ID) || dlqRowExists(t, s, item.ID) {
		t.Error("failed move left a partial change")
	}
}

// -------------------------------------------------------------------------
// DERIVED ORPHAN BYTES
// -------------------------------------------------------------------------

// TestSqlite_OrphanBytes_CountQueuedAndDeadLettered asserts a backend's
// orphan bytes are the sum of its queued and dead-lettered rows, read the
// same through backend_capacity and GetQuotaStats, and drop when the rows are
// retired.
func TestSqlite_OrphanBytes_CountQueuedAndDeadLettered(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	queued := queueCleanup(t, s, cleanupBackend, "k-queued", 100)
	dead := queueCleanup(t, s, cleanupBackend, "k-dead", 200)
	queueCleanup(t, s, "backend-b", "k-other", 50)
	if moved, err := s.MoveCleanupToDLQ(ctx, &dead, "gave up"); err != nil || !moved {
		t.Fatalf("MoveCleanupToDLQ = %v, %v", moved, err)
	}
	assertOrphanBytes(t, s, cleanupBackend, 300)
	assertOrphanBytes(t, s, "backend-b", 50)

	if _, err := s.CompleteCleanupItems(ctx, []core.CleanupItem{queued}); err != nil {
		t.Fatalf("CompleteCleanupItems: %v", err)
	}
	assertOrphanBytes(t, s, cleanupBackend, 200)

	if n, err := s.RequeueCleanupDLQ(ctx, cleanupBackend); err != nil || n != 1 {
		t.Fatalf("RequeueCleanupDLQ = %d, %v", n, err)
	}
	assertOrphanBytes(t, s, cleanupBackend, 200)
	var requeued []core.CleanupItem
	for _, item := range claimAll(t, s, takeoverInstance) {
		if item.BackendName == cleanupBackend {
			requeued = append(requeued, item)
		}
	}
	if retired, err := s.CompleteCleanupItems(ctx, requeued); err != nil || len(retired) != 1 {
		t.Fatalf("CompleteCleanupItems(requeued) = %v, %v; want one id", retired, err)
	}
	assertOrphanBytes(t, s, cleanupBackend, 0)
	assertOrphanBytes(t, s, "backend-b", 50)
}

// assertOrphanBytes checks backend's orphan bytes read through
// backend_capacity and GetQuotaStats both equal want.
func assertOrphanBytes(t *testing.T, s *Store, backend string, want int64) {
	t.Helper()
	if got := orphanBytes(t, s, backend); got != want {
		t.Errorf("backend_capacity orphan_bytes(%s) = %d, want %d", backend, got, want)
	}
	stats, err := s.GetQuotaStats(context.Background())
	if err != nil {
		t.Fatalf("GetQuotaStats: %v", err)
	}
	if got := stats[backend].OrphanBytes; got != want {
		t.Errorf("GetQuotaStats orphan_bytes(%s) = %d, want %d", backend, got, want)
	}
}

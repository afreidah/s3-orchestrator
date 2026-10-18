// -------------------------------------------------------------------------------
// SQLite Cleanup Queue Claim Pattern Tests
//
// Author: Alex Freidah
//
// Mirrors the Postgres claim-pattern integration tests at the SQLite layer.
// SQLite serialises writes intrinsically so the FOR UPDATE SKIP LOCKED
// concurrency property is replaced by a single sequential check; the rest
// of the contract  -  rows queued already claimed, reclaim after grace,
// idempotent complete, retry clears claim, orphan bytes derived from the
// rows  -  applies identically and is exercised here.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// cleanupRowSize is the size every queued test row carries.
const cleanupRowSize = int64(256)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// queueCleanup queues a cleanup row for key on backend through the tx
// adapter, the way a transaction that stops recording bytes does, and returns
// its id. The row comes back claimed by the store's instance.
func queueCleanup(t *testing.T, s *Store, backend, key string, size int64) int64 {
	t.Helper()
	var id int64
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx core.TxAdapter) error {
		var err error
		id, err = tx.InsertCleanup(ctx, &core.CleanupRequest{
			BackendName: backend, ObjectKey: key, StorageKey: key, Reason: "test", SizeBytes: size,
		})
		return err
	}); err != nil {
		t.Fatalf("InsertCleanup(%s): %v", key, err)
	}
	return id
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
	releaseCleanupClaim(t, s, queueCleanup(t, s, backend, key, cleanupRowSize))
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
	if queued[0].ClaimedBy == nil || *queued[0].ClaimedBy != testInstanceID {
		t.Errorf("queued row claimed by %v, want %s", queued[0].ClaimedBy, testInstanceID)
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
// carries the store's instance id and a claim time, and stores the path the
// request names.
func TestSqlite_InsertCleanup_QueuesClaimedByInstance(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	id := queueCleanup(t, s, "backend-a", "k1", cleanupRowSize)

	pending, err := s.GetPendingCleanups(context.Background(), 10)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("pending = %+v, want the one queued row %d", pending, id)
	}
	got := pending[0]
	if got.ClaimedAt == nil {
		t.Error("ClaimedAt is nil, want the queue time")
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != testInstanceID {
		t.Errorf("ClaimedBy = %v, want %s", got.ClaimedBy, testInstanceID)
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
	id := queueCleanup(t, s, "backend-a", "k1", cleanupRowSize)

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
	if len(after) != 1 || after[0].ID != id || !after[0].Reclaimed {
		t.Fatalf("claimed = %+v, want row %d reclaimed", after, id)
	}
}

// TestSqlite_InsertCleanup_NoInstanceLeavesClaimantNull asserts a store
// opened without an instance id queues rows with no claimant.
func TestSqlite_InsertCleanup_NoInstanceLeavesClaimantNull(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	a := &sqliteTxAdapter{q: s.db}
	id, err := a.InsertCleanup(context.Background(), &core.CleanupRequest{
		BackendName: "backend-a", ObjectKey: "k1", StorageKey: "k1", Reason: "test", SizeBytes: 1,
	})
	if err != nil {
		t.Fatalf("InsertCleanup: %v", err)
	}
	var claimedBy *string
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT claimed_by FROM cleanup_queue WHERE id = ?`, id).Scan(&claimedBy); err != nil {
		t.Fatalf("read claimed_by: %v", err)
	}
	if claimedBy != nil {
		t.Errorf("claimed_by = %q, want NULL", *claimedBy)
	}
}

// -------------------------------------------------------------------------
// CLAIM
// -------------------------------------------------------------------------

// TestSqlite_ClaimPendingCleanups_StampsClaim asserts a claim of an
// unclaimed row returns it with Reclaimed=false and stamps claimed_at and
// claimed_by such that a follow-up GetPendingCleanups can read them back.
func TestSqlite_ClaimPendingCleanups_StampsClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mustEnqueueCleanup(t, s, "backend-a", "k1")

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

	pending, err := s.GetPendingCleanups(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending len = %d, want 1", len(pending))
	}
	if pending[0].ClaimedAt == nil {
		t.Errorf("ClaimedAt is nil after claim")
	}
	if pending[0].ClaimedBy == nil || *pending[0].ClaimedBy != "instance-X" {
		t.Errorf("ClaimedBy = %v, want instance-X", pending[0].ClaimedBy)
	}
}

// TestSqlite_ClaimPendingCleanups_ReclaimAfterGrace asserts a stale claim
// is reclaimable and the returned item carries Reclaimed=true.
func TestSqlite_ClaimPendingCleanups_ReclaimAfterGrace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	id := queueCleanup(t, s, "backend-a", "k1", cleanupRowSize)

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

// -------------------------------------------------------------------------
// COMPLETE AND RETRY
// -------------------------------------------------------------------------

// TestSqlite_CompleteCleanupItem_RetiresRowAndOrphans asserts completing a
// row removes it and its bytes from the backend's orphans, and a re-complete
// of the missing row is a no-op.
func TestSqlite_CompleteCleanupItem_RetiresRowAndOrphans(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	const size = int64(4096)
	id := queueCleanup(t, s, "backend-a", "k1", size)
	if got := orphanBytes(t, s, "backend-a"); got != size {
		t.Fatalf("orphan_bytes = %d with the row queued, want %d", got, size)
	}

	if err := s.CompleteCleanupItem(ctx, id); err != nil {
		t.Fatalf("CompleteCleanupItem: %v", err)
	}
	if got := orphanBytes(t, s, "backend-a"); got != 0 {
		t.Errorf("orphan_bytes = %d after complete, want 0", got)
	}
	if depth, _ := s.CleanupQueueDepth(ctx); depth != 0 {
		t.Errorf("queue depth = %d after complete, want 0", depth)
	}

	if err := s.CompleteCleanupItem(ctx, id); err != nil {
		t.Fatalf("CompleteCleanupItem (idempotent): %v", err)
	}
}

// TestSqlite_RetryCleanupItem_ClearsClaim asserts that a retry clears
// claimed_at/claimed_by so the next worker tick can re-claim immediately.
func TestSqlite_RetryCleanupItem_ClearsClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	id := queueCleanup(t, s, "backend-a", "k1", cleanupRowSize)

	// A backoff already in the past makes the row unambiguously due, so the
	// assertion below is about the claim being cleared rather than about
	// whether SQLite's NOW() advanced past a sub-millisecond retry stamp.
	if err := s.RetryCleanupItem(ctx, id, -time.Second, "transient"); err != nil {
		t.Fatalf("RetryCleanupItem: %v", err)
	}

	pending, err := s.GetPendingCleanups(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending len = %d, want 1", len(pending))
	}
	if pending[0].ClaimedAt != nil {
		t.Errorf("ClaimedAt = %v after retry, want nil", *pending[0].ClaimedAt)
	}
	if pending[0].ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v after retry, want nil", *pending[0].ClaimedBy)
	}
}

// TestSqlite_SweepStaleCleanupQueueRows_ClosedStoreErrors verifies the
// sweep's delete error is returned rather than reported as zero rows.
func TestSqlite_SweepStaleCleanupQueueRows_ClosedStoreErrors(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	s.Close()
	if _, err := s.SweepStaleCleanupQueueRows(context.Background(), "k", "backend-a"); err == nil {
		t.Error("expected an error from a closed store")
	}
}

// -------------------------------------------------------------------------
// DERIVED ORPHAN BYTES
// -------------------------------------------------------------------------

// TestSqlite_OrphanBytes_CountQueuedAndDeadLettered asserts a backend's
// orphan bytes are the sum of its queued and dead-lettered rows, read the
// same through backend_capacity, GetQuotaStats and ListBackendQuotaUsage,
// and drop when the rows are retired.
func TestSqlite_OrphanBytes_CountQueuedAndDeadLettered(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	queued := queueCleanup(t, s, "backend-a", "k-queued", 100)
	dead := queueCleanup(t, s, "backend-a", "k-dead", 200)
	queueCleanup(t, s, "backend-b", "k-other", 50)
	if moved, err := s.MoveCleanupToDLQ(ctx, dead, "gave up"); err != nil || !moved {
		t.Fatalf("MoveCleanupToDLQ = %v, %v", moved, err)
	}
	assertOrphanBytes(t, s, "backend-a", 300)
	assertOrphanBytes(t, s, "backend-b", 50)

	if err := s.CompleteCleanupItem(ctx, queued); err != nil {
		t.Fatalf("CompleteCleanupItem: %v", err)
	}
	assertOrphanBytes(t, s, "backend-a", 200)

	if n, err := s.RequeueCleanupDLQ(ctx, "backend-a"); err != nil || n != 1 {
		t.Fatalf("RequeueCleanupDLQ = %d, %v", n, err)
	}
	assertOrphanBytes(t, s, "backend-a", 200)
	pending, err := s.GetPendingCleanups(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	for _, item := range pending {
		if item.BackendName != "backend-a" {
			continue
		}
		if err := s.CompleteCleanupItem(ctx, item.ID); err != nil {
			t.Fatalf("CompleteCleanupItem(%d): %v", item.ID, err)
		}
	}
	assertOrphanBytes(t, s, "backend-a", 0)
	assertOrphanBytes(t, s, "backend-b", 50)
}

// assertOrphanBytes checks backend's orphan bytes read through
// backend_capacity, GetQuotaStats and ListBackendQuotaUsage all equal want.
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
	usage, err := s.ListBackendQuotaUsage(context.Background())
	if err != nil {
		t.Fatalf("ListBackendQuotaUsage: %v", err)
	}
	for _, u := range usage {
		if u.BackendName == backend && u.OrphanBytes != want {
			t.Errorf("ListBackendQuotaUsage orphan_bytes(%s) = %d, want %d", backend, u.OrphanBytes, want)
		}
	}
}

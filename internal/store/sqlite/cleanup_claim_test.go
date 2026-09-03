// -------------------------------------------------------------------------------
// SQLite Cleanup Queue Claim Pattern Tests
//
// Author: Alex Freidah
//
// Mirrors the Postgres claim-pattern integration tests at the SQLite layer.
// SQLite serialises writes intrinsically so the FOR UPDATE SKIP LOCKED
// concurrency property is replaced by a single sequential check; the rest
// of the contract  -  reclaim after grace, atomic complete with orphan_bytes
// decrement and idempotent re-complete, retry clears claim  -  applies
// identically and is exercised here.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"testing"
	"time"
)

// TestSqlite_ClaimPendingCleanups_StampsClaim asserts a fresh claim returns
// the row with Reclaimed=false and stamps claimed_at/claimed_by such that a
// follow-up GetPendingCleanups can read them back.
func TestSqlite_ClaimPendingCleanups_StampsClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueCleanup(ctx, "backend-a", "k1", "test", 256); err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}

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

	if err := s.EnqueueCleanup(ctx, "backend-a", "k1", "test", 256); err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}
	first, err := s.ClaimPendingCleanups(ctx, 10, "A", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first claim len = %d, want 1", len(first))
	}
	id := first[0].ID

	// Backdate the claim_at so the next claim sees it as stale.
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

// TestSqlite_CompleteCleanupItem_AtomicDecrement asserts the row is removed
// AND orphan_bytes is decremented by size_bytes in a single call. A
// re-complete on the (now-missing) row is a no-op  -  no double-decrement.
func TestSqlite_CompleteCleanupItem_AtomicDecrement(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	const size = int64(4096)
	if err := s.IncrementOrphanBytes(ctx, "backend-a", size); err != nil {
		t.Fatalf("seed orphan_bytes: %v", err)
	}
	before := readOrphanBytesSqlite(t, s, "backend-a")

	if err := s.EnqueueCleanup(ctx, "backend-a", "k1", "test", size); err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}
	pending, err := s.GetPendingCleanups(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	id := pending[0].ID

	if err := s.CompleteCleanupItem(ctx, id); err != nil {
		t.Fatalf("CompleteCleanupItem: %v", err)
	}
	after := readOrphanBytesSqlite(t, s, "backend-a")
	if before-after != size {
		t.Errorf("orphan_bytes delta = %d, want %d", before-after, size)
	}

	if err := s.CompleteCleanupItem(ctx, id); err != nil {
		t.Fatalf("CompleteCleanupItem (idempotent): %v", err)
	}
	if after2 := readOrphanBytesSqlite(t, s, "backend-a"); after2 != after {
		t.Errorf("orphan_bytes drifted on idempotent re-complete: %d -> %d", after, after2)
	}
}

// TestSqlite_CompleteCleanupItem_ClampsAtZero asserts orphan_bytes never
// goes negative even when size_bytes exceeds the current counter.
func TestSqlite_CompleteCleanupItem_ClampsAtZero(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.db.ExecContext(ctx,
		`UPDATE backend_quotas SET orphan_bytes = 100 WHERE backend_name = 'backend-a'`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.EnqueueCleanup(ctx, "backend-a", "k1", "test", 10_000); err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}
	pending, err := s.GetPendingCleanups(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	if err := s.CompleteCleanupItem(ctx, pending[0].ID); err != nil {
		t.Fatalf("CompleteCleanupItem: %v", err)
	}
	if got := readOrphanBytesSqlite(t, s, "backend-a"); got != 0 {
		t.Errorf("orphan_bytes = %d, want 0", got)
	}
}

// TestSqlite_RetryCleanupItem_ClearsClaim asserts that a retry clears
// claimed_at/claimed_by so the next worker tick can re-claim immediately.
func TestSqlite_RetryCleanupItem_ClearsClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.EnqueueCleanup(ctx, "backend-a", "k1", "test", 256); err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}
	claimed, err := s.ClaimPendingCleanups(ctx, 10, "A", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	id := claimed[0].ID

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

// readOrphanBytesSqlite returns the current orphan_bytes for backendName.
func readOrphanBytesSqlite(t *testing.T, s *Store, backendName string) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT orphan_bytes FROM backend_quotas WHERE backend_name = ?`, backendName,
	).Scan(&v); err != nil {
		t.Fatalf("read orphan_bytes: %v", err)
	}
	return v
}

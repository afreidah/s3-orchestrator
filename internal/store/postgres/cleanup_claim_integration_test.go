// -----------------------------------------------------------------------------
// Cleanup Queue Claim Pattern Integration Tests
//
// Author: Alex Freidah
//
// Exercises the FOR UPDATE SKIP LOCKED claim path, the grace-cutoff reclaim
// rule, and the atomic CompleteCleanupItem CTE against a real Postgres
// container. These behaviours are the load-bearing pieces of the cleanup
// race fix and are not adequately covered by unit tests with mocks.
// -----------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestStoreInt_ClaimPendingCleanups_ConcurrentDisjoint asserts that two
// concurrent ClaimPendingCleanups transactions return disjoint row sets.
// Without FOR UPDATE SKIP LOCKED, both calls would return the same rows;
// the disjoint-set property is the correctness guarantee that prevents the
// double-decrement of orphan_bytes that motivated this change.
func TestStoreInt_ClaimPendingCleanups_ConcurrentDisjoint(t *testing.T) {
	s := adapterPgStore(t)

	const rows = 10
	enqueueN(t, s, rows, "claim-disjoint")
	results := claimInParallel(t, s, 2, rows, time.Now().Add(-time.Hour))
	assertDisjointAndCleanup(t, s, results)
}

// claimResult captures one parallel claim's outcome.
type claimResult struct {
	ids []int64
	err error
}

// enqueueN inserts n cleanup_queue rows with the given uniqueKey suffix.
func enqueueN(t *testing.T, s *Store, n int, suffix string) {
	t.Helper()
	for range n {
		if err := s.EnqueueCleanup(context.Background(), "backend-a", uniqueKey(t, suffix), "test", 256); err != nil {
			t.Fatalf("EnqueueCleanup: %v", err)
		}
	}
}

// claimInParallel kicks off n concurrent ClaimPendingCleanups calls and
// returns each goroutine's result indexed by goroutine number.
func claimInParallel(t *testing.T, s *Store, workers, limit int, graceCutoff time.Time) []claimResult {
	t.Helper()
	results := make([]claimResult, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			items, err := s.ClaimPendingCleanups(context.Background(), limit, "instance-test", graceCutoff)
			results[i] = claimResultFromItems(items, err)
		})
	}
	wg.Wait()
	return results
}

// claimResultFromItems collects ids from a claim batch into a claimResult.
func claimResultFromItems(items []core.CleanupItem, err error) claimResult {
	r := claimResult{err: err}
	for _, it := range items {
		r.ids = append(r.ids, it.ID)
	}
	return r
}

// assertDisjointAndCleanup fails the test if any id appears in more than
// one result, then completes every claimed row so the shared fixture
// does not leak rows into subsequent tests.
func assertDisjointAndCleanup(t *testing.T, s *Store, results []claimResult) {
	t.Helper()
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("claim %d: %v", i, r.err)
		}
	}
	seen := make(map[int64]int)
	for i, r := range results {
		for _, id := range r.ids {
			if prev, dup := seen[id]; dup {
				t.Errorf("row id %d returned by both claim %d and claim %d", id, prev, i)
			}
			seen[id] = i
		}
	}
	for id := range seen {
		_ = s.CompleteCleanupItem(context.Background(), id)
	}
}

// TestStoreInt_ClaimPendingCleanups_ReclaimAfterGrace asserts that a row
// whose claim is older than the grace cutoff is reclaimable, and that the
// returned item carries Reclaimed=true so the caller can drive the
// stale-claim-recovered metric.
func TestStoreInt_ClaimPendingCleanups_ReclaimAfterGrace(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	key := uniqueKey(t, "reclaim")
	if err := s.EnqueueCleanup(ctx, "backend-a", key, "test", 256); err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}

	item := claimByKey(t, s, "instance-A", time.Now().Add(-time.Hour), key)
	if item.Reclaimed {
		t.Errorf("first claim of fresh row marked Reclaimed=true")
	}
	defer func() { _ = s.CompleteCleanupItem(ctx, item.ID) }()

	// Ten-minute-old claim under a 5m cutoff must reclaim with Reclaimed=true.
	backdateClaim(t, s, item.ID, "10 minutes")
	second := mustClaim(t, s, "instance-B", time.Now().Add(-5*time.Minute))
	got := findByID(second, item.ID)
	if got == nil {
		t.Errorf("backdated row was not reclaimed")
	} else if !got.Reclaimed {
		t.Errorf("reclaimed row Reclaimed=false, want true")
	}

	// Same backdated row under a 15m cutoff must NOT be reclaimed.
	backdateClaim(t, s, item.ID, "10 minutes")
	third := mustClaim(t, s, "instance-C", time.Now().Add(-15*time.Minute))
	if findByID(third, item.ID) != nil {
		t.Errorf("row reclaimed under a 15m grace despite 10m-old claim")
	}
}

// mustClaim wraps ClaimPendingCleanups + t.Fatal on error so callers stay focused on assertions.
func mustClaim(t *testing.T, s *Store, instanceID string, graceCutoff time.Time) []core.CleanupItem {
	t.Helper()
	items, err := s.ClaimPendingCleanups(context.Background(), 100, instanceID, graceCutoff)
	if err != nil {
		t.Fatalf("ClaimPendingCleanups(%s): %v", instanceID, err)
	}
	return items
}

// claimByKey claims and returns the row matching key; fails the test if the row is missing from the batch.
func claimByKey(t *testing.T, s *Store, instanceID string, graceCutoff time.Time, key string) core.CleanupItem {
	t.Helper()
	for _, it := range mustClaim(t, s, instanceID, graceCutoff) {
		if it.ObjectKey == key {
			return it
		}
	}
	t.Fatalf("expected to claim key %s", key)
	return core.CleanupItem{}
}

// findByID returns the matching item from a claim batch, or nil if absent.
func findByID(items []core.CleanupItem, id int64) *core.CleanupItem {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
	}
	return nil
}

// backdateClaim sets claimed_at to NOW() - interval so the next claim sees the row as stale.
func backdateClaim(t *testing.T, s *Store, id int64, interval string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE cleanup_queue SET claimed_at = NOW() - $1::interval WHERE id = $2`,
		interval, id,
	); err != nil {
		t.Fatalf("backdate claim: %v", err)
	}
}

// TestStoreInt_CompleteCleanupItem_AtomicDecrement asserts CompleteCleanupItem
// deletes the row AND decrements orphan_bytes for the backing backend in a
// single round-trip. A second call against the (now-missing) row must be a
// no-op  -  no double-decrement.
func TestStoreInt_CompleteCleanupItem_AtomicDecrement(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	const size = int64(4096)
	if err := s.IncrementOrphanBytes(ctx, "backend-a", size); err != nil {
		t.Fatalf("seed orphan_bytes: %v", err)
	}
	before := readOrphanBytes(t, s, "backend-a")

	key := uniqueKey(t, "atomic-complete")
	if err := s.EnqueueCleanup(ctx, "backend-a", key, "test", size); err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}

	pending, err := s.GetPendingCleanups(ctx, 200)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	var id int64
	for _, p := range pending {
		if p.ObjectKey == key {
			id = p.ID
			break
		}
	}
	if id == 0 {
		t.Fatalf("row for %s not found", key)
	}

	if err := s.CompleteCleanupItem(ctx, id); err != nil {
		t.Fatalf("CompleteCleanupItem: %v", err)
	}
	after := readOrphanBytes(t, s, "backend-a")
	if before-after != size {
		t.Errorf("orphan_bytes delta = %d, want %d", before-after, size)
	}

	// Idempotent: a second complete on a deleted row must not move the
	// counter further.
	if err := s.CompleteCleanupItem(ctx, id); err != nil {
		t.Fatalf("CompleteCleanupItem (second): %v", err)
	}
	if after2 := readOrphanBytes(t, s, "backend-a"); after2 != after {
		t.Errorf("orphan_bytes drifted on idempotent re-complete: before=%d after=%d", after, after2)
	}
}

// TestStoreInt_CompleteCleanupItem_ClampsAtZero asserts that orphan_bytes is
// clamped to 0 even when the cleanup row's recorded size_bytes exceeds the
// current counter. GREATEST(0, ...) in the SQL is the load-bearing piece.
func TestStoreInt_CompleteCleanupItem_ClampsAtZero(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	if _, err := s.pool.Exec(ctx,
		`UPDATE backend_quotas SET orphan_bytes = 100 WHERE backend_name = 'backend-a'`,
	); err != nil {
		t.Fatalf("reset orphan_bytes: %v", err)
	}

	key := uniqueKey(t, "clamp-zero")
	if err := s.EnqueueCleanup(ctx, "backend-a", key, "test", 10_000); err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}

	pending, err := s.GetPendingCleanups(ctx, 200)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	var id int64
	for _, p := range pending {
		if p.ObjectKey == key {
			id = p.ID
			break
		}
	}
	if id == 0 {
		t.Fatalf("row for %s not found", key)
	}

	if err := s.CompleteCleanupItem(ctx, id); err != nil {
		t.Fatalf("CompleteCleanupItem: %v", err)
	}
	if got := readOrphanBytes(t, s, "backend-a"); got != 0 {
		t.Errorf("orphan_bytes = %d, want 0 (clamped)", got)
	}
}

// TestStoreInt_RetryCleanupItem_ClearsClaim asserts that scheduling a retry
// clears claimed_at and claimed_by so the row is immediately re-eligible
// for the next worker tick rather than waiting out the grace period.
func TestStoreInt_RetryCleanupItem_ClearsClaim(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	key := uniqueKey(t, "retry-clears")
	if err := s.EnqueueCleanup(ctx, "backend-a", key, "test", 256); err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}

	claimed, err := s.ClaimPendingCleanups(ctx, 100, "instance-A", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	var id int64
	for _, c := range claimed {
		if c.ObjectKey == key {
			id = c.ID
			break
		}
	}
	if id == 0 {
		t.Fatalf("row for %s not claimed", key)
	}

	// A backoff already in the past makes the row unambiguously due, so the
	// assertion below is about the claim being cleared rather than about
	// whether NOW() advanced past a sub-millisecond retry stamp.
	if err := s.RetryCleanupItem(ctx, id, -time.Second, "transient"); err != nil {
		t.Fatalf("RetryCleanupItem: %v", err)
	}

	again, err := s.ClaimPendingCleanups(ctx, 100, "instance-B", time.Now())
	if err != nil {
		t.Fatalf("claim again: %v", err)
	}
	var seen bool
	for _, c := range again {
		if c.ID == id {
			seen = true
			if c.Reclaimed {
				t.Errorf("row reclaimed=true after RetryCleanupItem cleared its claim, want false")
			}
			break
		}
	}
	if !seen {
		t.Errorf("row not eligible after RetryCleanupItem cleared the claim")
	}
	_ = s.CompleteCleanupItem(ctx, id)
}

// readOrphanBytes returns the current orphan_bytes value for backendName.
func readOrphanBytes(t *testing.T, s *Store, backendName string) int64 {
	t.Helper()
	var v int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT orphan_bytes FROM backend_quotas WHERE backend_name = $1`, backendName,
	).Scan(&v); err != nil {
		t.Fatalf("read orphan_bytes: %v", err)
	}
	return v
}

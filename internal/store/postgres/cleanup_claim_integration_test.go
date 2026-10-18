// -------------------------------------------------------------------------------
// Cleanup Queue Claim Pattern Integration Tests
//
// Author: Alex Freidah
//
// Exercises the FOR UPDATE SKIP LOCKED claim path, the claim a queued row is
// born with, the grace-cutoff reclaim rule, row retirement, and the orphan
// bytes backend_capacity derives from the queue against a real Postgres
// container. These behaviours are the load-bearing pieces of the cleanup
// outbox and are not adequately covered by unit tests with mocks.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestStoreInt_ClaimPendingCleanups_ConcurrentDisjoint asserts that two
// concurrent ClaimPendingCleanups transactions return disjoint row sets.
// Without FOR UPDATE SKIP LOCKED, both calls would return the same rows and
// two workers would delete the same bytes.
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

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// enqueueN queues n unclaimed cleanup_queue rows with the given uniqueKey
// suffix.
func enqueueN(t *testing.T, s *Store, n int, suffix string) {
	t.Helper()
	for range n {
		queueUnclaimed(t, s, cleanupOf("backend-a", uniqueKey(t, suffix), "test", 256))
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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestStoreInt_InsertCleanup_ClaimedUntilGrace asserts a row a transaction
// queues is born claimed by the store's instance, is left alone by a worker
// claim while that claim is fresh, and is reclaimed with Reclaimed set once
// the claim ages past the grace cutoff.
func TestStoreInt_InsertCleanup_ClaimedUntilGrace(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "outbox")
	id := queueCleanup(t, s, cleanupOf("backend-a", key, "test", 256))
	defer func() { _ = s.CompleteCleanupItem(ctx, id) }()

	var claimedBy string
	var claimedAt time.Time
	if err := s.pool.QueryRow(ctx, `SELECT claimed_by, claimed_at FROM cleanup_queue WHERE id = $1`, id).Scan(&claimedBy, &claimedAt); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if claimedBy != pgFixtureInstance {
		t.Errorf("claimed_by = %q, want %q", claimedBy, pgFixtureInstance)
	}
	if time.Since(claimedAt) > time.Minute {
		t.Errorf("claimed_at = %v, want the insert time", claimedAt)
	}

	if findByID(mustClaim(t, s, "instance-B", time.Now().Add(-5*time.Minute)), id) != nil {
		t.Fatal("a worker claimed a row whose queueing claim is still fresh")
	}
	backdateClaim(t, s, id, "10 minutes")
	got := findByID(mustClaim(t, s, "instance-B", time.Now().Add(-5*time.Minute)), id)
	if got == nil {
		t.Fatal("a row whose queueing claim aged past the grace was not reclaimed")
	}
	if !got.Reclaimed {
		t.Error("reclaimed row Reclaimed=false, want true")
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
	queueUnclaimed(t, s, cleanupOf("backend-a", key, "test", 256))

	item := claimByKey(t, s, "instance-A", time.Now().Add(-time.Hour), key)
	if item.Reclaimed {
		t.Errorf("first claim of an unclaimed row marked Reclaimed=true")
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

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// mustClaim wraps ClaimPendingCleanups + t.Fatal on error so callers stay focused on assertions.
func mustClaim(t *testing.T, s *Store, instanceID string, graceCutoff time.Time) []core.CleanupItem {
	t.Helper()
	items, err := s.ClaimPendingCleanups(context.Background(), 1000, instanceID, graceCutoff)
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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestStoreInt_CompleteCleanupItem_RetiresRow asserts CompleteCleanupItem
// deletes the row, and that a second call against the missing row is a
// no-op.
func TestStoreInt_CompleteCleanupItem_RetiresRow(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	id := queueCleanup(t, s, cleanupOf("backend-a", uniqueKey(t, "complete"), "test", 4096))

	if err := s.CompleteCleanupItem(ctx, id); err != nil {
		t.Fatalf("CompleteCleanupItem: %v", err)
	}
	if pgCount(t, s, `SELECT COUNT(*) FROM cleanup_queue WHERE id = $1`, id) != 0 {
		t.Errorf("row %d still queued after completion", id)
	}
	if err := s.CompleteCleanupItem(ctx, id); err != nil {
		t.Errorf("CompleteCleanupItem (second): %v", err)
	}
}

// TestStoreInt_OrphanBytes_DerivedFromQueueAndDLQ asserts a backend's orphan
// bytes are the sum of its queued and dead-lettered rows in backend_capacity,
// GetQuotaStats and ListBackendQuotaUsage alike, that they count against the
// bytes the backend has available, and that a retired row stops counting.
func TestStoreInt_OrphanBytes_DerivedFromQueueAndDLQ(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	backend := uniqueKey(t, "backend")
	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{{Name: backend, QuotaBytes: 1000}}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	queued := queueCleanup(t, s, cleanupOf(backend, uniqueKey(t, "q"), "test", 100))
	dead := queueCleanup(t, s, cleanupOf(backend, uniqueKey(t, "d"), "test", 40))
	if moved, err := s.MoveCleanupToDLQ(ctx, dead, "gave up"); err != nil || !moved {
		t.Fatalf("MoveCleanupToDLQ = %v, %v", moved, err)
	}
	defer func() { _, _ = s.pool.Exec(ctx, `DELETE FROM cleanup_dlq WHERE backend_name = $1`, backend) }()

	assertOrphanBytes(t, s, backend, 140)
	if avail := readAvailableBytes(t, s, backend); avail != 860 {
		t.Errorf("available_bytes = %d, want 860 with 140 orphan bytes", avail)
	}

	if err := s.CompleteCleanupItem(ctx, queued); err != nil {
		t.Fatalf("CompleteCleanupItem: %v", err)
	}
	assertOrphanBytes(t, s, backend, 40)
}

// TestStoreInt_SweepStaleCleanupQueueRows_ByPath asserts a sweep removes every
// queued row for the path on the backend, returns how many went, and leaves
// rows for other paths and backends.
func TestStoreInt_SweepStaleCleanupQueueRows_ByPath(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "sweep")
	queueCleanup(t, s, cleanupOf("backend-a", key, "test", 10))
	queueCleanup(t, s, cleanupOf("backend-a", key, "test", 20))
	otherPath := queueCleanup(t, s, &core.CleanupRequest{BackendName: "backend-a", ObjectKey: key, StorageKey: key + "!w2", Reason: "test", SizeBytes: 1})
	otherBackend := queueCleanup(t, s, cleanupOf("backend-b", key, "test", 1))
	defer func() {
		_ = s.CompleteCleanupItem(ctx, otherPath)
		_ = s.CompleteCleanupItem(ctx, otherBackend)
	}()

	n, err := s.SweepStaleCleanupQueueRows(ctx, key, "backend-a")
	if err != nil {
		t.Fatalf("SweepStaleCleanupQueueRows: %v", err)
	}
	if n != 2 {
		t.Errorf("swept %d rows, want 2", n)
	}
	if left := pgCount(t, s, `SELECT COUNT(*) FROM cleanup_queue WHERE id = ANY($1)`, []int64{otherPath, otherBackend}); left != 2 {
		t.Errorf("%d of the other rows left, want both", left)
	}
	if again, err := s.SweepStaleCleanupQueueRows(ctx, key, "backend-a"); err != nil || again != 0 {
		t.Errorf("second sweep = %d, %v; want 0", again, err)
	}
}

// TestStoreInt_RetryCleanupItem_ClearsClaim asserts that scheduling a retry
// clears claimed_at and claimed_by so the row is immediately re-eligible
// for the next worker tick rather than waiting out the grace period.
func TestStoreInt_RetryCleanupItem_ClearsClaim(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	id := queueCleanup(t, s, cleanupOf("backend-a", uniqueKey(t, "retry-clears"), "test", 256))

	// A backoff already in the past makes the row unambiguously due, so the
	// assertion below is about the claim being cleared rather than about
	// whether NOW() advanced past a sub-millisecond retry stamp.
	if err := s.RetryCleanupItem(ctx, id, -time.Second, "transient"); err != nil {
		t.Fatalf("RetryCleanupItem: %v", err)
	}

	got := findByID(mustClaim(t, s, "instance-B", time.Now().Add(-time.Hour)), id)
	if got == nil {
		t.Fatal("row not eligible after RetryCleanupItem cleared the claim")
	}
	if got.Reclaimed {
		t.Errorf("row reclaimed=true after RetryCleanupItem cleared its claim, want false")
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
	_ = s.CompleteCleanupItem(ctx, id)
}

// assertOrphanBytes checks the backend's orphan bytes read the same through
// backend_capacity, GetQuotaStats and ListBackendQuotaUsage.
func assertOrphanBytes(t *testing.T, s *Store, backend string, want int64) {
	t.Helper()
	ctx := context.Background()
	var view int64
	if err := s.pool.QueryRow(ctx, `SELECT orphan_bytes FROM backend_capacity WHERE backend_name = $1`, backend).Scan(&view); err != nil {
		t.Fatalf("read backend_capacity: %v", err)
	}
	stats, err := s.GetQuotaStats(ctx)
	if err != nil {
		t.Fatalf("GetQuotaStats: %v", err)
	}
	usage, err := s.ListBackendQuotaUsage(ctx)
	if err != nil {
		t.Fatalf("ListBackendQuotaUsage: %v", err)
	}
	var listed int64 = -1
	for _, u := range usage {
		if u.BackendName == backend {
			listed = u.OrphanBytes
		}
	}
	if view != want || stats[backend].OrphanBytes != want || listed != want {
		t.Errorf("orphan bytes = view %d, stats %d, usage %d; want %d", view, stats[backend].OrphanBytes, listed, want)
	}
}

// readAvailableBytes returns the bytes backend_capacity reports free on the
// backend.
func readAvailableBytes(t *testing.T, s *Store, backend string) int64 {
	t.Helper()
	var v int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT available_bytes FROM backend_capacity WHERE backend_name = $1`, backend,
	).Scan(&v); err != nil {
		t.Fatalf("read available_bytes: %v", err)
	}
	return v
}

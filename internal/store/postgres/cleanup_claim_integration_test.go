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
	"fmt"
	"maps"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// instanceA and instanceB are the worker instances that claim rows, and
// staleAge is how far backdateClaim ages a claim past a 5m grace cutoff.
const (
	instanceA = "instance-A"
	instanceB = "instance-B"
	staleAge  = "10 minutes"
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
// one result, then drops every claimed row so the shared fixture does not
// leak rows into subsequent tests.
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
	dropCleanupRows(t, s, slices.Collect(maps.Keys(seen))...)
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestStoreInt_InsertCleanup_ClaimedUntilGrace asserts a row a transaction
// queues is born claimed by the store's instance with the claim it returns,
// is left alone by a worker claim while that claim is fresh, and is reclaimed
// with Reclaimed set once the claim ages past the grace cutoff.
func TestStoreInt_InsertCleanup_ClaimedUntilGrace(t *testing.T) {
	s := adapterPgStore(t)
	key := uniqueKey(t, "outbox")
	item := queueCleanup(t, s, cleanupOf("backend-a", key, "test", 256))
	id := item.ID
	defer dropCleanupRows(t, s, id)

	stored := readClaim(t, s, id)
	if stored.By != pgFixtureInstance {
		t.Errorf("claimed_by = %q, want %q", stored.By, pgFixtureInstance)
	}
	if time.Since(stored.At) > time.Minute {
		t.Errorf("claimed_at = %v, want the insert time", stored.At)
	}
	if item.Claim == nil || !sameClaim(*item.Claim, stored) {
		t.Errorf("returned claim = %+v, want the stored claim %+v", item.Claim, stored)
	}

	if findByID(mustClaim(t, s, instanceB, time.Now().Add(-5*time.Minute)), id) != nil {
		t.Fatal("a worker claimed a row whose queueing claim is still fresh")
	}
	backdateClaim(t, s, id, staleAge)
	got := findByID(mustClaim(t, s, instanceB, time.Now().Add(-5*time.Minute)), id)
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

	key := uniqueKey(t, "reclaim")
	queueUnclaimed(t, s, cleanupOf("backend-a", key, "test", 256))

	item := claimByKey(t, s, instanceA, time.Now().Add(-time.Hour), key)
	if item.Reclaimed {
		t.Errorf("first claim of an unclaimed row marked Reclaimed=true")
	}
	defer dropCleanupRows(t, s, item.ID)

	// Ten-minute-old claim under a 5m cutoff must reclaim with Reclaimed=true.
	backdateClaim(t, s, item.ID, staleAge)
	second := mustClaim(t, s, instanceB, time.Now().Add(-5*time.Minute))
	got := findByID(second, item.ID)
	if got == nil {
		t.Errorf("backdated row was not reclaimed")
	} else if !got.Reclaimed {
		t.Errorf("reclaimed row Reclaimed=false, want true")
	}

	// Same backdated row under a 15m cutoff must NOT be reclaimed.
	backdateClaim(t, s, item.ID, staleAge)
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

// readClaim returns the claim stored on the queued row with id, with an empty
// By for a NULL claimed_by.
func readClaim(t *testing.T, s *Store, id int64) core.CleanupClaim {
	t.Helper()
	var c core.CleanupClaim
	if err := s.pool.QueryRow(context.Background(),
		`SELECT claimed_at, COALESCE(claimed_by, '') FROM cleanup_queue WHERE id = $1`, id,
	).Scan(&c.At, &c.By); err != nil {
		t.Fatalf("read claim %d: %v", id, err)
	}
	return c
}

// sameClaim reports whether two claims name the same stamp by the same
// instance.
func sameClaim(a, b core.CleanupClaim) bool {
	return a.At.Equal(b.At) && a.By == b.By
}

// queuedAttempts returns the attempt count on the queued row with id.
func queuedAttempts(t *testing.T, s *Store, id int64) int32 {
	t.Helper()
	var n int32
	if err := s.pool.QueryRow(context.Background(), `SELECT attempts FROM cleanup_queue WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("read attempts %d: %v", id, err)
	}
	return n
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestStoreInt_CompleteCleanupItems_RetiresRows asserts CompleteCleanupItems
// deletes every row held under its item's claim in one call and returns their
// ids, and that a second call against the retired rows retires nothing.
func TestStoreInt_CompleteCleanupItems_RetiresRows(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	items := []core.CleanupItem{
		queueCleanup(t, s, cleanupOf("backend-a", uniqueKey(t, "complete-1"), "test", 4096)),
		queueCleanup(t, s, cleanupOf("backend-b", uniqueKey(t, "complete-2"), "test", 1)),
	}
	ids := []int64{items[0].ID, items[1].ID}

	retired, err := s.CompleteCleanupItems(ctx, items)
	if err != nil {
		t.Fatalf("CompleteCleanupItems: %v", err)
	}
	slices.Sort(retired)
	if !slices.Equal(retired, ids) {
		t.Errorf("retired = %v, want %v", retired, ids)
	}
	if pgCount(t, s, `SELECT COUNT(*) FROM cleanup_queue WHERE id = ANY($1)`, ids) != 0 {
		t.Errorf("rows %v still queued after completion", ids)
	}
	again, err := s.CompleteCleanupItems(ctx, items)
	if err != nil || len(again) != 0 {
		t.Errorf("CompleteCleanupItems (second) = %v, %v; want none", again, err)
	}
}

// TestStoreInt_CompleteCleanupItems_EmptyIsNoOp asserts an empty batch
// retires nothing and reports no error.
func TestStoreInt_CompleteCleanupItems_EmptyIsNoOp(t *testing.T) {
	s := adapterPgStore(t)
	retired, err := s.CompleteCleanupItems(context.Background(), nil)
	if err != nil || retired != nil {
		t.Errorf("CompleteCleanupItems(nil) = %v, %v; want nil, nil", retired, err)
	}
}

// TestStoreInt_OrphanBytes_DerivedFromQueueAndDLQ asserts a backend's orphan
// bytes are the sum of its queued and dead-lettered rows in backend_capacity
// and GetQuotaStats alike, that they count against the bytes the backend has
// available, and that a retired row stops counting.
func TestStoreInt_OrphanBytes_DerivedFromQueueAndDLQ(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	backend := uniqueKey(t, "backend")
	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{{Name: backend, QuotaBytes: 1000}}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	queued := queueCleanup(t, s, cleanupOf(backend, uniqueKey(t, "q"), "test", 100))
	dead := queueCleanup(t, s, cleanupOf(backend, uniqueKey(t, "d"), "test", 40))
	if moved, err := s.MoveCleanupToDLQ(ctx, &dead, "gave up"); err != nil || !moved {
		t.Fatalf("MoveCleanupToDLQ = %v, %v", moved, err)
	}
	defer func() { _, _ = s.pool.Exec(ctx, `DELETE FROM cleanup_dlq WHERE backend_name = $1`, backend) }()

	assertOrphanBytes(t, s, backend, 140)
	if avail := readAvailableBytes(t, s, backend); avail != 860 {
		t.Errorf("available_bytes = %d, want 860 with 140 orphan bytes", avail)
	}

	if _, err := s.CompleteCleanupItems(ctx, []core.CleanupItem{queued}); err != nil {
		t.Fatalf("CompleteCleanupItems: %v", err)
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
	otherPath := queueCleanup(t, s, &core.CleanupRequest{BackendName: "backend-a", ObjectKey: key, StorageKey: key + "!w2", Reason: "test", SizeBytes: 1}).ID
	otherBackend := queueCleanup(t, s, cleanupOf("backend-b", key, "test", 1)).ID
	defer dropCleanupRows(t, s, otherPath, otherBackend)

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

	item := queueCleanup(t, s, cleanupOf("backend-a", uniqueKey(t, "retry-clears"), "test", 256))
	id := item.ID
	defer dropCleanupRows(t, s, id)

	// A backoff already in the past makes the row unambiguously due, so the
	// assertion below is about the claim being cleared rather than about
	// whether NOW() advanced past a sub-millisecond retry stamp.
	if ok, err := s.RetryCleanupItem(ctx, &item, -time.Second, "transient"); err != nil || !ok {
		t.Fatalf("RetryCleanupItem = %v, %v; want true", ok, err)
	}

	got := findByID(mustClaim(t, s, instanceB, time.Now().Add(-time.Hour)), id)
	if got == nil {
		t.Fatal("row not eligible after RetryCleanupItem cleared the claim")
	}
	if got.Reclaimed {
		t.Errorf("row reclaimed=true after RetryCleanupItem cleared its claim, want false")
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
}

// assertOrphanBytes checks the backend's orphan bytes read the same through
// backend_capacity and GetQuotaStats.
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
	if view != want || stats[backend].OrphanBytes != want {
		t.Errorf("orphan bytes = view %d, stats %d; want %d", view, stats[backend].OrphanBytes, want)
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

// TestStoreInt_ClaimPendingCleanups_ReturnsStampedClaim asserts each claimed
// item carries the claim the statement stamped on its row.
func TestStoreInt_ClaimPendingCleanups_ReturnsStampedClaim(t *testing.T) {
	s := adapterPgStore(t)
	key := uniqueKey(t, "stamped")
	id := queueUnclaimed(t, s, cleanupOf("backend-a", key, "test", 8))
	defer dropCleanupRows(t, s, id)

	item := claimByKey(t, s, instanceA, time.Now().Add(-time.Hour), key)
	if item.Claim == nil {
		t.Fatal("claimed item has no claim")
	}
	if stored := readClaim(t, s, id); !sameClaim(*item.Claim, stored) || stored.By != instanceA {
		t.Errorf("returned claim %+v, stored claim %+v; want the same stamp by %q", *item.Claim, stored, instanceA)
	}
}

// TestStoreInt_GetPendingCleanups_ClaimFromRow asserts the listing reports a
// claimed row's stored claim and no claim for an unclaimed row.
func TestStoreInt_GetPendingCleanups_ClaimFromRow(t *testing.T) {
	s := adapterPgStore(t)
	claimed := queueCleanup(t, s, cleanupOf("backend-a", uniqueKey(t, "claimed"), "test", 8))
	unclaimed := queueUnclaimed(t, s, cleanupOf("backend-a", uniqueKey(t, "unclaimed"), "test", 8))
	defer dropCleanupRows(t, s, claimed.ID, unclaimed)

	pending, err := s.GetPendingCleanups(context.Background(), math.MaxInt32)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	held, free := findByID(pending, claimed.ID), findByID(pending, unclaimed)
	if held == nil || free == nil {
		t.Fatalf("listing missing rows: claimed=%v unclaimed=%v", held, free)
	}
	if held.Claim == nil || !sameClaim(*held.Claim, *claimed.Claim) {
		t.Errorf("claimed row claim = %+v, want %+v", held.Claim, *claimed.Claim)
	}
	if free.Claim != nil {
		t.Errorf("unclaimed row claim = %+v, want nil", *free.Claim)
	}
	if held.Reclaimed || free.Reclaimed {
		t.Error("listing marked a row Reclaimed, want false")
	}
}

// TestStoreInt_CleanupSettle_LostClaimIsFenced asserts that once another
// instance reclaims rows whose claim aged out, the first holder's complete,
// retry and dead-letter writes touch nothing and report the claim lost, while
// the new holder's writes settle the rows.
func TestStoreInt_CleanupSettle_LostClaimIsFenced(t *testing.T) {
	s := adapterPgStore(t)
	stale := make([]core.CleanupItem, 3)
	ids := make([]int64, len(stale))
	for i := range stale {
		stale[i] = queueCleanup(t, s, cleanupOf("backend-a", uniqueKey(t, fmt.Sprintf("fenced-%d", i)), "test", 64))
		ids[i] = stale[i].ID
		backdateClaim(t, s, ids[i], staleAge)
	}
	defer dropCleanupRows(t, s, ids...)
	defer dropDLQRows(t, s, ids...)

	claimed := mustClaim(t, s, instanceB, time.Now().Add(-5*time.Minute))
	fresh := make([]core.CleanupItem, len(stale))
	for i, id := range ids {
		got := findByID(claimed, id)
		if got == nil {
			t.Fatalf("row %d was not reclaimed", id)
		}
		fresh[i] = *got
	}

	assertLostClaimSettlesNothing(t, s, stale)
	assertHeldClaimSettles(t, s, stale, fresh)
}

// assertLostClaimSettlesNothing settles each stale item one way and checks
// every write reports the claim lost and leaves its row queued untouched.
func assertLostClaimSettlesNothing(t *testing.T, s *Store, stale []core.CleanupItem) {
	t.Helper()
	ctx := context.Background()
	if retired, err := s.CompleteCleanupItems(ctx, stale[:1]); err != nil || len(retired) != 0 {
		t.Errorf("stale CompleteCleanupItems = %v, %v; want none", retired, err)
	}
	if ok, err := s.RetryCleanupItem(ctx, &stale[1], time.Minute, "late"); err != nil || ok {
		t.Errorf("stale RetryCleanupItem = %v, %v; want false", ok, err)
	}
	if moved, err := s.MoveCleanupToDLQ(ctx, &stale[2], "late"); err != nil || moved {
		t.Errorf("stale MoveCleanupToDLQ = %v, %v; want false", moved, err)
	}
	for i := range stale {
		if got := queuedAttempts(t, s, stale[i].ID); got != 0 {
			t.Errorf("row %d attempts = %d after a lost settle, want 0", stale[i].ID, got)
		}
		if c := readClaim(t, s, stale[i].ID); c.By != instanceB {
			t.Errorf("row %d claimed by %q after a lost settle, want %q", stale[i].ID, c.By, instanceB)
		}
	}
	if n := pgCount(t, s, `SELECT COUNT(*) FROM cleanup_dlq WHERE original_id = $1`, stale[2].ID); n != 0 {
		t.Errorf("lost dead-letter wrote %d DLQ rows, want 0", n)
	}
}

// assertHeldClaimSettles settles each row under the new holder's claim and
// checks every write lands. The complete batch mixes in a stale item for
// another row, which must be left queued.
func assertHeldClaimSettles(t *testing.T, s *Store, stale, fresh []core.CleanupItem) {
	t.Helper()
	ctx := context.Background()
	retired, err := s.CompleteCleanupItems(ctx, []core.CleanupItem{stale[1], fresh[0]})
	if err != nil || !slices.Equal(retired, []int64{fresh[0].ID}) {
		t.Errorf("CompleteCleanupItems = %v, %v; want only [%d]", retired, err, fresh[0].ID)
	}
	if ok, err := s.RetryCleanupItem(ctx, &fresh[1], time.Minute, "transient"); err != nil || !ok {
		t.Errorf("RetryCleanupItem = %v, %v; want true", ok, err)
	}
	if got := queuedAttempts(t, s, fresh[1].ID); got != 1 {
		t.Errorf("retried row attempts = %d, want 1", got)
	}
	if moved, err := s.MoveCleanupToDLQ(ctx, &fresh[2], "gave up"); err != nil || !moved {
		t.Errorf("MoveCleanupToDLQ = %v, %v; want true", moved, err)
	}
	if n := pgCount(t, s, `SELECT COUNT(*) FROM cleanup_queue WHERE id = ANY($1)`, []int64{fresh[0].ID, fresh[2].ID}); n != 0 {
		t.Errorf("%d settled rows still queued, want 0", n)
	}
}

// dropDLQRows deletes the dead-lettered rows whose original ids are ids.
func dropDLQRows(t *testing.T, s *Store, ids ...int64) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM cleanup_dlq WHERE original_id = ANY($1)`, ids); err != nil {
		t.Fatalf("drop DLQ rows %v: %v", ids, err)
	}
}

// TestStoreInt_MoveCleanupToDLQ_CarriesRow asserts a dead-letter moves only
// the item's row, keeping its id as original_id, its created_at as
// first_enqueued_at and the new last_error, while its bytes still count.
func TestStoreInt_MoveCleanupToDLQ_CarriesRow(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "dlq")
	dead := queueCleanup(t, s, &core.CleanupRequest{BackendName: "backend-b", ObjectKey: key, StorageKey: key + "!w3", Reason: "delete_failed", SizeBytes: 77})
	kept := queueCleanup(t, s, cleanupOf("backend-b", key, "delete_failed", 1))
	defer dropCleanupRows(t, s, kept.ID)
	defer dropDLQRows(t, s, dead.ID)
	if _, err := s.pool.Exec(ctx, `UPDATE cleanup_queue SET attempts = 4, last_error = 'earlier' WHERE id = $1`, dead.ID); err != nil {
		t.Fatalf("seed attempts: %v", err)
	}
	var created time.Time
	if err := s.pool.QueryRow(ctx, `SELECT created_at FROM cleanup_queue WHERE id = $1`, dead.ID).Scan(&created); err != nil {
		t.Fatalf("read created_at: %v", err)
	}

	if moved, err := s.MoveCleanupToDLQ(ctx, &dead, "gave up"); err != nil || !moved {
		t.Fatalf("MoveCleanupToDLQ = %v, %v; want true", moved, err)
	}

	var got core.CleanupDLQItem
	var origID int64
	if err := s.pool.QueryRow(ctx,
		`SELECT original_id, backend_name, object_key, storage_key, reason, size_bytes, attempts, first_enqueued_at, last_error
		   FROM cleanup_dlq WHERE original_id = $1`, dead.ID,
	).Scan(&origID, &got.BackendName, &got.ObjectKey, &got.StorageKey, &got.Reason, &got.SizeBytes, &got.Attempts, &got.FirstEnqueued, &got.LastError); err != nil {
		t.Fatalf("read DLQ row: %v", err)
	}
	if origID != dead.ID || got.BackendName != "backend-b" || got.ObjectKey != key || got.StorageKey != key+"!w3" ||
		got.Reason != "delete_failed" || got.SizeBytes != 77 || got.Attempts != 4 || got.LastError != "gave up" {
		t.Errorf("DLQ row = id %d %+v, want the queued row's fields with last_error %q", origID, got, "gave up")
	}
	if !got.FirstEnqueued.Equal(created) {
		t.Errorf("first_enqueued_at = %v, want the queue row's created_at %v", got.FirstEnqueued, created)
	}
	if pgCount(t, s, `SELECT COUNT(*) FROM cleanup_queue WHERE id = $1`, dead.ID) != 0 {
		t.Error("dead-lettered row still queued")
	}
	if pgCount(t, s, `SELECT COUNT(*) FROM cleanup_queue WHERE id = $1`, kept.ID) != 1 {
		t.Error("dead-letter removed a row it was not given")
	}
}

// TestStoreInt_CleanupSettle_EmptyInstanceMatches asserts a claim with an
// empty By matches both a row queued with a NULL claimed_by and a row claimed
// under an empty instance id.
func TestStoreInt_CleanupSettle_EmptyInstanceMatches(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	anon := &pgTxAdapter{q: s.queries}
	nullBy := make([]core.CleanupItem, 3)
	ids := make([]int64, 0, len(nullBy)+1)
	for i := range nullBy {
		item, err := anon.InsertCleanup(ctx, cleanupOf("backend-a", uniqueKey(t, fmt.Sprintf("anon-%d", i)), "test", 2))
		if err != nil {
			t.Fatalf("InsertCleanup: %v", err)
		}
		nullBy[i] = item
		ids = append(ids, item.ID)
	}
	key := uniqueKey(t, "empty-by")
	ids = append(ids, queueUnclaimed(t, s, cleanupOf("backend-a", key, "test", 2)))
	defer dropCleanupRows(t, s, ids...)
	defer dropDLQRows(t, s, ids...)

	emptyBy := claimByKey(t, s, "", time.Now().Add(-time.Hour), key)
	if emptyBy.Claim == nil || emptyBy.Claim.By != "" {
		t.Fatalf("claim under an empty instance = %+v, want an empty By", emptyBy.Claim)
	}
	retired, err := s.CompleteCleanupItems(ctx, []core.CleanupItem{nullBy[0], emptyBy})
	slices.Sort(retired)
	if want := []int64{nullBy[0].ID, emptyBy.ID}; err != nil || !slices.Equal(retired, want) {
		t.Errorf("CompleteCleanupItems = %v, %v; want %v", retired, err, want)
	}
	if ok, err := s.RetryCleanupItem(ctx, &nullBy[1], time.Minute, "transient"); err != nil || !ok {
		t.Errorf("RetryCleanupItem = %v, %v; want true", ok, err)
	}
	if moved, err := s.MoveCleanupToDLQ(ctx, &nullBy[2], "gave up"); err != nil || !moved {
		t.Errorf("MoveCleanupToDLQ = %v, %v; want true", moved, err)
	}
}

// TestStoreInt_CleanupSettle_NilClaimMatchesNothing asserts an item with no
// claim settles no row, since every row a settle reaches was claimed.
func TestStoreInt_CleanupSettle_NilClaimMatchesNothing(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	item := queueCleanup(t, s, cleanupOf("backend-a", uniqueKey(t, "nil-claim"), "test", 2))
	defer dropCleanupRows(t, s, item.ID)
	defer dropDLQRows(t, s, item.ID)
	item.Claim = nil

	if retired, err := s.CompleteCleanupItems(ctx, []core.CleanupItem{item}); err != nil || len(retired) != 0 {
		t.Errorf("CompleteCleanupItems = %v, %v; want none", retired, err)
	}
	if ok, err := s.RetryCleanupItem(ctx, &item, time.Minute, "x"); err != nil || ok {
		t.Errorf("RetryCleanupItem = %v, %v; want false", ok, err)
	}
	if moved, err := s.MoveCleanupToDLQ(ctx, &item, "x"); err != nil || moved {
		t.Errorf("MoveCleanupToDLQ = %v, %v; want false", moved, err)
	}
	if got := queuedAttempts(t, s, item.ID); got != 0 {
		t.Errorf("attempts = %d, want the row untouched", got)
	}
}

// TestStoreInt_CleanupQueue_MaxAttemptsExcluded asserts a row whose attempts
// reached core.MaxCleanupAttempts is left out of the listing, the claim and
// the queue depth, while a row one attempt short is still included.
func TestStoreInt_CleanupQueue_MaxAttemptsExcluded(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	under := queueUnclaimed(t, s, cleanupOf("backend-a", uniqueKey(t, "under"), "test", 2))
	over := queueUnclaimed(t, s, cleanupOf("backend-a", uniqueKey(t, "over"), "test", 2))
	defer dropCleanupRows(t, s, under, over)
	setAttempts(t, s, under, core.MaxCleanupAttempts-1)
	before := mustDepth(t, s)
	setAttempts(t, s, over, core.MaxCleanupAttempts)

	if after := mustDepth(t, s); after != before-1 {
		t.Errorf("depth = %d after exhausting a row, want %d", after, before-1)
	}
	pending, err := s.GetPendingCleanups(ctx, math.MaxInt32)
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	if findByID(pending, under) == nil || findByID(pending, over) != nil {
		t.Errorf("listing has under=%v over=%v, want only the row under the ceiling", findByID(pending, under) != nil, findByID(pending, over) != nil)
	}
	claimed := mustClaim(t, s, instanceA, time.Now().Add(-time.Hour))
	if findByID(claimed, under) == nil || findByID(claimed, over) != nil {
		t.Errorf("claim has under=%v over=%v, want only the row under the ceiling", findByID(claimed, under) != nil, findByID(claimed, over) != nil)
	}
}

// setAttempts sets the attempt count on the queued row with id.
func setAttempts(t *testing.T, s *Store, id int64, n int) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `UPDATE cleanup_queue SET attempts = $1 WHERE id = $2`, n, id); err != nil {
		t.Fatalf("set attempts %d: %v", id, err)
	}
}

// mustDepth returns CleanupQueueDepth, failing the test on error.
func mustDepth(t *testing.T, s *Store) int64 {
	t.Helper()
	n, err := s.CleanupQueueDepth(context.Background())
	if err != nil {
		t.Fatalf("CleanupQueueDepth: %v", err)
	}
	return n
}

// TestStoreInt_CleanupQueue_QueryErrorsWrapped asserts every cleanup queue
// method returns an error when its query fails.
func TestStoreInt_CleanupQueue_QueryErrorsWrapped(t *testing.T) {
	s := &Store{queries: closedPgAdapter(t, adapterPgStore(t)).q}
	ctx := context.Background()
	item := core.CleanupItem{ID: 1, Claim: &core.CleanupClaim{At: time.Now(), By: instanceA}}
	if _, err := s.GetPendingCleanups(ctx, 1); err == nil {
		t.Error("GetPendingCleanups: expected error")
	}
	if _, err := s.ClaimPendingCleanups(ctx, 1, instanceA, time.Now()); err == nil {
		t.Error("ClaimPendingCleanups: expected error")
	}
	if _, err := s.CompleteCleanupItems(ctx, []core.CleanupItem{item}); err == nil {
		t.Error("CompleteCleanupItems: expected error")
	}
	if _, err := s.RetryCleanupItem(ctx, &item, time.Minute, "x"); err == nil {
		t.Error("RetryCleanupItem: expected error")
	}
	if _, err := s.MoveCleanupToDLQ(ctx, &item, "x"); err == nil {
		t.Error("MoveCleanupToDLQ: expected error")
	}
	if _, err := s.CleanupQueueDepth(ctx); err == nil {
		t.Error("CleanupQueueDepth: expected error")
	}
}

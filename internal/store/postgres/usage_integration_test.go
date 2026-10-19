// -------------------------------------------------------------------------------
// Quota Admission and Reconcile Integration Tests
//
// Author: Alex Freidah
//
// Pins the two guarantees that keep a backend's striped byte total honest under
// concurrency. A claim on a backend with a ceiling takes a transaction lock on
// the backend before its room check, so concurrent claims cannot each miss the
// other's bytes and overrun it; an unlimited backend takes no lock. Reconcile
// brings the striped total to the ledger sum by adding the difference, so a
// write that commits alongside keeps its own adjustment.
//
// Every test runs on backends of its own, because the container is shared and
// reconcile corrects every backend at once.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

const (
	// claimSize is the byte size of every intent the claim tests admit.
	claimSize int64 = 100

	// lockWaitTimeout bounds every wait on another transaction's lock.
	lockWaitTimeout = 10 * time.Second
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// quotaPgBackend registers a backend of the test's own with the given ceiling,
// zero meaning unlimited, and removes its rows when the test ends.
func quotaPgBackend(t *testing.T, s *Store, suffix string, limit int64) string {
	t.Helper()
	ctx := context.Background()
	name := uniqueKey(t, suffix)
	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{{Name: name, QuotaBytes: limit}}); err != nil {
		t.Fatalf("SyncQuotaLimits(%s): %v", name, err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"pending_objects", "object_locations", "backend_quota_stripes"} {
			_, _ = s.pool.Exec(context.Background(), `DELETE FROM `+table+` WHERE backend_name = $1`, name)
		}
	})
	return name
}

// claimOn runs a claim of claimSize bytes on backend through the store.
func claimOn(ctx context.Context, s *Store, backend, intentID string) (bool, error) {
	return s.InsertPendingIfFits(ctx, &core.PendingObject{
		IntentID: backend + "#" + intentID, ObjectKey: backend + "/" + intentID,
		StorageKey: backend + "/" + intentID, BackendName: backend, SizeBytes: claimSize,
	}, core.AcceptForWrite)
}

// claimLocks counts the backend claim locks on backend, held or awaited.
func claimLocks(t *testing.T, s *Store, backend string, granted bool) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND objsubid = 2
		  AND classid = $1::int::oid AND objid = hashtext($2)::oid AND granted = $3`,
		core.LockSpaceBackendClaim, backend, granted,
	).Scan(&n); err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return n
}

// waitFor polls cond until it holds, failing the test after lockWaitTimeout.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(lockWaitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// beginPgAdapter opens a transaction the test commits or rolls back itself,
// rolled back at the end if it is still open.
func beginPgAdapter(t *testing.T, s *Store) (*pgTxAdapter, func(commit bool)) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	end := func(commit bool) {
		if !commit {
			_ = tx.Rollback(ctx)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("Commit: %v", err)
		}
	}
	return &pgTxAdapter{q: s.queries.WithTx(tx), instanceID: s.instanceID}, end
}

// admission is what a claim run in the background reported.
type admission struct {
	fits bool
	err  error
}

// claimInBackground starts a claim on backend and returns where its result
// arrives.
func claimInBackground(ctx context.Context, s *Store, backend, intentID string) <-chan admission {
	done := make(chan admission, 1)
	go func() {
		fits, err := claimOn(ctx, s, backend, intentID)
		done <- admission{fits: fits, err: err}
	}()
	return done
}

// -------------------------------------------------------------------------
// CLAIM LOCK
// -------------------------------------------------------------------------

// TestPgClaim_ConcurrentClaimsCannotOverrunLimit launches more claims than a
// backend has room for, all at once, and verifies exactly as many succeed as
// fit under its ceiling.
func TestPgClaim_ConcurrentClaimsCannotOverrunLimit(t *testing.T) {
	s := adapterPgStore(t)
	const room, claims = 3, 12
	backend := quotaPgBackend(t, s, "limited", room*claimSize)

	var admitted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range claims {
		wg.Go(func() {
			<-start
			fits, err := claimOn(context.Background(), s, backend, fmt.Sprintf("c%d", i))
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
			}
			if fits {
				admitted.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()

	if got := admitted.Load(); got != room {
		t.Errorf("admitted %d claims, want exactly the %d that fit", got, room)
	}
}

// TestPgClaim_LockBlocksSecondClaimOnLimitedBackend holds the backend lock in
// one transaction and verifies a claim on the same backend waits for it, then
// proceeds once the holder ends.
func TestPgClaim_LockBlocksSecondClaimOnLimitedBackend(t *testing.T) {
	s := adapterPgStore(t)
	backend := quotaPgBackend(t, s, "limited", 10*claimSize)
	holder, end := beginPgAdapter(t, s)
	if err := holder.AcquireBackendLock(context.Background(), backend); err != nil {
		t.Fatalf("AcquireBackendLock: %v", err)
	}
	if claimLocks(t, s, backend, true) != 1 {
		t.Fatal("the holder's backend lock is not granted")
	}

	ctx, cancel := context.WithTimeout(context.Background(), lockWaitTimeout)
	defer cancel()
	done := claimInBackground(ctx, s, backend, "waiter")
	waitFor(t, "the second claim to wait on the backend lock", func() bool {
		return claimLocks(t, s, backend, false) == 1
	})
	select {
	case r := <-done:
		t.Fatalf("claim finished (%v, %v) while the lock was held", r.fits, r.err)
	default:
	}

	end(false)
	if r := <-done; r.err != nil || !r.fits {
		t.Errorf("claim after release = %v, %v; want admitted", r.fits, r.err)
	}
}

// TestPgClaim_UnlimitedBackendTakesNoLock verifies a claim on a backend with no
// ceiling neither takes the backend lock nor waits on one another transaction
// holds.
func TestPgClaim_UnlimitedBackendTakesNoLock(t *testing.T) {
	s := adapterPgStore(t)
	backend := quotaPgBackend(t, s, "unlimited", 0)

	claimer, endClaimer := beginPgAdapter(t, s)
	if err := claimer.AcquireBackendLock(context.Background(), backend); err != nil {
		t.Fatalf("AcquireBackendLock: %v", err)
	}
	if n := claimLocks(t, s, backend, true); n != 0 {
		t.Errorf("unlimited backend holds %d claim locks, want none", n)
	}
	endClaimer(false)

	ctx, cancel := context.WithTimeout(context.Background(), lockWaitTimeout)
	defer cancel()
	holder, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int, hashtext($2))`,
		core.LockSpaceBackendClaim, backend); err != nil {
		t.Fatalf("hold the backend's lock key: %v", err)
	}

	if fits, err := claimOn(ctx, s, backend, "unblocked"); err != nil || !fits {
		t.Errorf("claim beside a held lock = %v, %v; want admitted without waiting", fits, err)
	}
}

// -------------------------------------------------------------------------
// RECONCILE
// -------------------------------------------------------------------------

// recordOn records an object of size bytes on backend, which charges its
// stripe alongside the ledger row.
func recordOn(t *testing.T, s *Store, backend, suffix string, size int64) {
	t.Helper()
	if _, _, err := s.RecordObject(context.Background(), &core.RecordObjectRequest{
		Key: uniqueKey(t, suffix), Copies: []core.ObjectCopy{{Backend: backend}}, Size: size,
	}); err != nil {
		t.Fatalf("RecordObject(%s): %v", suffix, err)
	}
}

// seedPgStripes replaces a backend's stripe rows with the given per-stripe
// values, so a test controls exactly what the striped total reads.
func seedPgStripes(t *testing.T, s *Store, backend string, stripes map[int16]int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `DELETE FROM backend_quota_stripes WHERE backend_name = $1`, backend); err != nil {
		t.Fatalf("clear stripes: %v", err)
	}
	for stripe, used := range stripes {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO backend_quota_stripes (backend_name, stripe_id, bytes_used) VALUES ($1, $2, $3)`,
			backend, stripe, used); err != nil {
			t.Fatalf("seed stripe %d: %v", stripe, err)
		}
	}
}

// pgRawStripe reads one stripe's value, or zero when the row does not exist.
func pgRawStripe(t *testing.T, s *Store, backend string, stripe int16) int64 {
	t.Helper()
	var used int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(bytes_used), 0)::bigint FROM backend_quota_stripes WHERE backend_name = $1 AND stripe_id = $2`,
		backend, stripe,
	).Scan(&used); err != nil {
		t.Fatalf("read stripe %d: %v", stripe, err)
	}
	return used
}

// pgRawStripeSum reads a backend's striped total without a clamp, which is the
// figure reconcile makes equal to the ledger.
func pgRawStripeSum(t *testing.T, s *Store, backend string) int64 {
	t.Helper()
	var used int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(bytes_used), 0)::bigint FROM backend_quota_stripes WHERE backend_name = $1`, backend,
	).Scan(&used); err != nil {
		t.Fatalf("read stripe total: %v", err)
	}
	return used
}

// mustPgReconcile runs ReconcileUsage and fails the test on an error.
func mustPgReconcile(t *testing.T, s *Store) map[string]int64 {
	t.Helper()
	adj, err := s.ReconcileUsage(context.Background())
	if err != nil {
		t.Fatalf("ReconcileUsage: %v", err)
	}
	return adj
}

// TestStoreInt_ReconcileUsage_CorrectsDriftAdditively skews a backend's counter
// across several stripes and verifies reconcile adds the difference to stripe
// zero, leaving the other stripes as they were, so the total equals the ledger.
func TestStoreInt_ReconcileUsage_CorrectsDriftAdditively(t *testing.T) {
	s := adapterPgStore(t)
	backend := quotaPgBackend(t, s, "skewed", 1<<30)
	recordOn(t, s, backend, "k1", 100)
	recordOn(t, s, backend, "k2", 250)
	seedPgStripes(t, s, backend, map[int16]int64{0: 1000, 3: -200, 7: 450})

	adj := mustPgReconcile(t, s)

	if got, ok := adj[backend]; !ok || got != 350-1250 {
		t.Errorf("delta = %d (reported %v), want %d", got, ok, 350-1250)
	}
	if got := pgRawStripeSum(t, s, backend); got != 350 {
		t.Errorf("striped total = %d, want the ledger's 350", got)
	}
	if got := pgRawStripe(t, s, backend, 0); got != 100 {
		t.Errorf("stripe 0 = %d, want 1000 - 900", got)
	}
	if pgRawStripe(t, s, backend, 3) != -200 || pgRawStripe(t, s, backend, 7) != 450 {
		t.Error("stripes other than zero changed, want them left as they were")
	}
}

// TestStoreInt_ReconcileUsage_NoDriftReturnsEmpty verifies an agreeing backend
// is not reported, and that a reconcile right after another finds nothing.
func TestStoreInt_ReconcileUsage_NoDriftReturnsEmpty(t *testing.T) {
	s := adapterPgStore(t)
	backend := quotaPgBackend(t, s, "agreeing", 1<<30)
	recordOn(t, s, backend, "k1", 100)

	if got, ok := mustPgReconcile(t, s)[backend]; ok {
		t.Errorf("agreeing backend reported a delta of %d", got)
	}
	if adj := mustPgReconcile(t, s); len(adj) != 0 {
		t.Errorf("second reconcile applied %v, want nothing", adj)
	}
	if got := pgRawStripeSum(t, s, backend); got != 100 {
		t.Errorf("striped total = %d, want the untouched 100", got)
	}
}

// TestStoreInt_ReconcileUsage_BackendWithoutCopiesGoesToZero verifies a backend
// with no ledger rows is brought to zero from either side: a positive total
// spread over stripes and a negative one.
func TestStoreInt_ReconcileUsage_BackendWithoutCopiesGoesToZero(t *testing.T) {
	s := adapterPgStore(t)
	over := quotaPgBackend(t, s, "over", 1<<30)
	under := quotaPgBackend(t, s, "under", 1<<30)
	seedPgStripes(t, s, over, map[int16]int64{2: 300, 9: 200})
	seedPgStripes(t, s, under, map[int16]int64{5: -80})

	adj := mustPgReconcile(t, s)

	if adj[over] != -500 || adj[under] != 80 {
		t.Errorf("deltas = %d and %d, want -500 and 80", adj[over], adj[under])
	}
	for _, backend := range []string{over, under} {
		if got := pgRawStripeSum(t, s, backend); got != 0 {
			t.Errorf("%s striped total = %d, want 0", backend, got)
		}
	}
}

// TestStoreInt_ReconcileUsage_ConcurrentWriteKeepsItsAdjustment holds a write
// open on stripe zero while reconcile runs, commits it once reconcile waits on
// the row, and verifies the write's charge survives: the total equals the
// ledger including the write, and the delta covers only the prior drift.
func TestStoreInt_ReconcileUsage_ConcurrentWriteKeepsItsAdjustment(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	backend := quotaPgBackend(t, s, "racing", 1<<30)
	recordOn(t, s, backend, "k1", 100)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO backend_quota_stripes (backend_name, stripe_id, bytes_used) VALUES ($1, 0, 500)
		ON CONFLICT (backend_name, stripe_id) DO UPDATE
		SET bytes_used = backend_quota_stripes.bytes_used + EXCLUDED.bytes_used`, backend); err != nil {
		t.Fatalf("seed drift: %v", err)
	}

	writer, end := beginPgAdapter(t, s)
	key := uniqueKey(t, "k2")
	if err := writer.InsertObjectLocation(ctx, &core.ObjectLocation{
		ObjectKey: key, BackendName: backend, StorageKey: key, SizeBytes: 200,
	}); err != nil {
		t.Fatalf("InsertObjectLocation: %v", err)
	}
	if err := writer.AdjustQuotaStripe(ctx, backend, 0, 200); err != nil {
		t.Fatalf("AdjustQuotaStripe: %v", err)
	}

	type result struct {
		adj map[string]int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		adj, err := s.ReconcileUsage(ctx)
		done <- result{adj: adj, err: err}
	}()
	waitFor(t, "reconcile to wait on the writer's stripe row", func() bool {
		var n int
		if err := s.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '-- name: ReconcileUsage%'`).Scan(&n); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		return n > 0
	})
	end(true)

	r := <-done
	if r.err != nil {
		t.Fatalf("ReconcileUsage: %v", r.err)
	}
	if r.adj[backend] != -500 {
		t.Errorf("delta = %d, want -500 for the prior drift alone", r.adj[backend])
	}
	if got := pgRawStripeSum(t, s, backend); got != 300 {
		t.Errorf("striped total = %d, want the ledger's 300 including the write", got)
	}
}

// TestStoreInt_ReconcileUsage_ErrorSurfaces verifies a failed statement is
// returned rather than reported as no drift.
func TestStoreInt_ReconcileUsage_ErrorSurfaces(t *testing.T) {
	s := adapterPgStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if adj, err := s.ReconcileUsage(ctx); err == nil {
		t.Errorf("ReconcileUsage = %v, nil; want the cancellation error", adj)
	}
}

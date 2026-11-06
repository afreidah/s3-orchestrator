//go:build integration

// -------------------------------------------------------------------------------
// Claims Waiting on a Backend Lock
//
// Author: Alex Freidah
//
// Claims on a backend with a ceiling run one at a time. Pins that the ones
// waiting do not sit on the connection pool, because every query in the process
// needs that pool and a full one stalls requests that have nothing to do with
// the claim. The test holds the lock itself, so the result does not depend on
// load or on how fast the host is.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

const (
	claimWaitBackend   = "claim-wait-backend"
	claimWaitPoolSize  = 8
	claimWaitClaims    = 3 * claimWaitPoolSize
	claimWaitQueryWait = 2 * time.Second
	holdBackendLock    = "SELECT pg_advisory_xact_lock($1::int, hashtext($2))"
)

// claimLockWaiters counts the transactions queued on the backend's claim lock.
func claimLockWaiters(ctx context.Context, t *testing.T, s *Store) int {
	t.Helper()
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND NOT granted AND objsubid = 2
		  AND classid = $1::int::oid AND objid = hashtext($2)::oid`,
		core.LockSpaceBackendClaim, claimWaitBackend).Scan(&n)
	if err != nil {
		t.Fatalf("count lock waiters: %v", err)
	}
	return n
}

// awaitClaimQueue returns once the queue on the claim lock has formed and stopped
// growing, so every claim that will reach the database has.
func awaitClaimQueue(ctx context.Context, t *testing.T, s *Store) {
	t.Helper()
	last, steady := 0, 0
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && steady < 5; {
		n := claimLockWaiters(ctx, t, s)
		if n > 0 && n == last {
			steady++
		} else {
			steady = 0
		}
		last = n
		time.Sleep(50 * time.Millisecond)
	}
	if steady < 5 {
		t.Fatalf("claim queue never settled, last saw %d waiters", last)
	}
}

// claimPlan returns the executed plan of a claim on backend, rolled back so the
// claim leaves nothing behind.
func claimPlan(ctx context.Context, t *testing.T, s *Store, backend string) []string {
	t.Helper()
	query, args := generatedStatement(t, func(q *db.Queries) error {
		_, err := q.InsertPendingObjectIfFits(ctx, pendingInsertParams(&core.PendingObject{
			IntentID: backend, ObjectKey: backend, StorageKey: backend, BackendName: backend, SizeBytes: 1,
		}, core.AcceptForWrite))
		return err
	})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	return plan
}

// claimSumsRan reports whether a claim on backend executed the read of the
// cleanup rows behind the room sums.
func claimSumsRan(ctx context.Context, t *testing.T, s *Store, backend string) bool {
	t.Helper()
	found, ran := false, false
	for _, line := range claimPlan(ctx, t, s, backend) {
		if strings.Contains(line, "cleanup_queue") {
			found = true
			ran = ran || !strings.Contains(line, "never executed")
		}
	}
	if !found {
		t.Fatalf("no read of cleanup_queue in the plan for %s", backend)
	}
	return ran
}

// registerBackends adds backends for one test, and removes them and any intents
// left on them when it ends.
func registerBackends(ctx context.Context, t *testing.T, s *Store, backends ...config.BackendConfig) {
	t.Helper()
	if err := s.SyncQuotaLimits(ctx, backends); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		for i := range backends {
			if _, err := s.pool.Exec(cleanup, "DELETE FROM pending_objects WHERE backend_name = $1", backends[i].Name); err != nil {
				t.Errorf("clear intents: %v", err)
			}
			if err := s.queries.DeleteQuota(cleanup, backends[i].Name); err != nil {
				t.Errorf("delete quota: %v", err)
			}
		}
	})
}

// holdLock runs query in a transaction of its own and leaves it open, holding
// the lock until the caller rolls it back or the test ends.
func holdLock(ctx context.Context, t *testing.T, s *Store, query string, args ...any) pgx.Tx {
	t.Helper()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, query, args...); err != nil {
		t.Fatalf("hold the lock: %v", err)
	}
	return tx
}

// awaitLockTaken reports whether a lock on name, in the given lock form, is
// granted within ten seconds.
func awaitLockTaken(ctx context.Context, t *testing.T, s *Store, form int, name string) bool {
	t.Helper()
	var taken bool
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !taken; {
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
			WHERE locktype = 'advisory' AND granted AND objsubid = $1 AND objid = hashtext($2)::oid)`,
			form, name).Scan(&taken); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return taken
}

// TestClaims_ReadTheSumsOnlyUnderACeiling verifies a claim on an unlimited
// backend never runs the room sums, whose cost grows with the cleanup backlog,
// while a claim on a backend with a ceiling does.
func TestClaims_ReadTheSumsOnlyUnderACeiling(t *testing.T) {
	const capped, unlimited = "claim-sums-capped", "claim-sums-unlimited"
	ctx := t.Context()
	s := adapterPgStore(t)
	registerBackends(ctx, t, s, config.BackendConfig{Name: capped, QuotaBytes: 1 << 30}, config.BackendConfig{Name: unlimited})
	if !claimSumsRan(ctx, t, s, capped) {
		t.Error("a claim under a ceiling did not read the sums")
	}
	if claimSumsRan(ctx, t, s, unlimited) {
		t.Error("a claim on an unlimited backend read the sums")
	}
}

// TestLocks_TakeTheirNamesInByteOrder verifies a statement locking several names
// takes them lowest first whatever order they arrive in, which is what keeps two
// transactions with overlapping sets from deadlocking: the high name is held
// elsewhere, so the low one is only taken if it comes first.
func TestLocks_TakeTheirNamesInByteOrder(t *testing.T) {
	const low, high = "lock-order-a", "lock-order-b"
	ctx := t.Context()
	s := adapterPgStore(t)
	registerBackends(ctx, t, s, config.BackendConfig{Name: low, QuotaBytes: 1}, config.BackendConfig{Name: high, QuotaBytes: 1})
	cases := map[string]struct {
		hold string
		args []any
		lock func(context.Context, core.TxAdapter) error
		form int
	}{
		"backends": {
			hold: holdBackendLock,
			args: []any{core.LockSpaceBackendClaim, high},
			lock: func(ctx context.Context, tx core.TxAdapter) error { return tx.AcquireBackendLock(ctx, high, low) },
			form: 2,
		},
		"keys": {
			hold: "SELECT pg_advisory_xact_lock(hashtext($1))",
			args: []any{high},
			lock: func(ctx context.Context, tx core.TxAdapter) error { return tx.AcquireKeyLock(ctx, high, low) },
			form: 1,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			holder := holdLock(ctx, t, s, tc.hold, tc.args...)
			var wg sync.WaitGroup
			wg.Go(func() {
				if err := s.WithTx(ctx, tc.lock); err != nil {
					t.Errorf("lock both: %v", err)
				}
			})
			if !awaitLockTaken(ctx, t, s, tc.form, low) {
				t.Errorf("%s was not locked while %s was held", low, high)
			}
			if err := holder.Rollback(ctx); err != nil {
				t.Fatalf("release %s: %v", high, err)
			}
			wg.Wait()
		})
	}
}

// TestClaims_WaitingLeavesTheConnectionPoolFree verifies a query still runs while
// more claims than the pool has connections wait on a backend's lock, and that
// every claim goes through once the lock is released.
func TestClaims_WaitingLeavesTheConnectionPoolFree(t *testing.T) {
	ctx := t.Context()
	fixture := adapterPgStore(t)
	conn := fixture.pool.Config().ConnConfig
	s, err := NewStore(ctx, &config.DatabaseConfig{
		Host:     conn.Host,
		Port:     int(conn.Port),
		Database: conn.Database,
		User:     conn.User,
		Password: conn.Password,
		SSLMode:  "disable",
		MaxConns: claimWaitPoolSize,
		MinConns: 1,
	}, nil, "claim-wait")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(s.Close)
	registerBackends(ctx, t, fixture, config.BackendConfig{Name: claimWaitBackend, QuotaBytes: 1 << 30})
	holder := holdLock(ctx, t, fixture, holdBackendLock, core.LockSpaceBackendClaim, claimWaitBackend)

	var wg sync.WaitGroup
	claimed := make([]bool, claimWaitClaims)
	for i := range claimWaitClaims {
		wg.Go(func() {
			id := fmt.Sprintf("claim-wait-%d", i)
			fits, err := s.InsertPendingIfFits(ctx, &core.PendingObject{
				IntentID: id, ObjectKey: id, StorageKey: id, BackendName: claimWaitBackend, SizeBytes: 1,
			}, core.AcceptForWrite)
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
			}
			claimed[i] = fits
		})
	}
	awaitClaimQueue(ctx, t, fixture)

	queryCtx, cancel := context.WithTimeout(ctx, claimWaitQueryWait)
	defer cancel()
	var one int
	if err := s.pool.QueryRow(queryCtx, "SELECT 1").Scan(&one); err != nil {
		t.Errorf("a query could not get a connection while claims waited: %v", err)
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the claim lock: %v", err)
	}
	wg.Wait()
	for i, fits := range claimed {
		if !fits {
			t.Errorf("claim %d was declined", i)
		}
	}
}

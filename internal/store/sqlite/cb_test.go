// Tests for the CB-aware *sql.DB wrapper. The breaker state machine
// itself is covered in internal/breaker; these tests prove the wrapper
// feeds PreCheck/PostCheck correctly and that wrapDB no-ops on a nil cb.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// newCB builds a database breaker with a tight failure threshold so a
// single error trips it.
func newCB(threshold int) *breaker.CircuitBreaker {
	return breaker.NewCircuitBreaker("test", threshold, time.Minute, isDBErrorForTest, core.ErrDBUnavailable)
}

// isDBErrorForTest treats any non-sentinel error as a DB error.
func isDBErrorForTest(err error) bool {
	if err == nil {
		return false
	}
	return !errors.Is(err, core.ErrObjectNotFound) && !errors.Is(err, core.ErrNoSpaceAvailable)
}

// openMemDB opens an empty in-memory sqlite for tests that need a real
// *sql.DB to wrap. The handle is closed by t.Cleanup.
func openMemDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestWrapDB_NilCBReturnsInner(t *testing.T) {
	t.Parallel()
	d := openMemDB(t)
	got := wrapDB(d, nil)
	if got != dbAPI(d) {
		t.Fatalf("wrapDB(_, nil) returned wrapper, want raw *sql.DB")
	}
}

func TestCBDB_ExecForwardsClosed(t *testing.T) {
	t.Parallel()
	d := openMemDB(t)
	w := wrapDB(d, newCB(3))
	if _, err := w.ExecContext(context.Background(), "CREATE TABLE t(a INT)"); err != nil {
		t.Fatalf("ExecContext: %v", err)
	}
}

func TestCBDB_ExecReturnsSentinelWhenOpen(t *testing.T) {
	t.Parallel()
	d := openMemDB(t)
	w := wrapDB(d, newCB(1))
	// First call: invalid SQL trips the breaker.
	if _, err := w.ExecContext(context.Background(), "this is not sql"); err == nil {
		t.Fatal("expected SQL error on trip call")
	}
	// Second call: open circuit short-circuits before reaching sqlite.
	_, err := w.ExecContext(context.Background(), "CREATE TABLE t(a INT)")
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("err = %v, want ErrDBUnavailable", err)
	}
}

func TestCBWithTx_ReturnsSentinelWhenOpen(t *testing.T) {
	t.Parallel()
	d := openMemDB(t)
	cb := newCB(1)
	w := wrapDB(d, cb)
	// Trip the breaker via the wrapper's Exec path.
	_, _ = w.ExecContext(context.Background(), "this is not sql")
	called := false
	err := cbWithTx(context.Background(), d, cb, func(_ *sql.Tx) error {
		called = true
		return nil
	})
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("cbWithTx err = %v, want ErrDBUnavailable", err)
	}
	if called {
		t.Fatal("fn should not run when breaker is open")
	}
}

func TestCBWithTx_NilCBSkipsBreakerAndCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openMemDB(t)
	if _, err := d.ExecContext(ctx, "CREATE TABLE t(a INT)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	err := cbWithTx(ctx, d, nil, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO t(a) VALUES (1)")
		return err
	})
	if err != nil {
		t.Fatalf("cbWithTx with nil cb: %v", err)
	}
	var n int
	if err := d.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1 (commit should have landed)", n)
	}
}

func TestCBWithTx_FnErrorRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openMemDB(t)
	if _, err := d.ExecContext(ctx, "CREATE TABLE t(a INT)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	want := errors.New("fn failed")
	err := cbWithTx(ctx, d, nil, func(tx *sql.Tx) error {
		_, _ = tx.ExecContext(ctx, "INSERT INTO t(a) VALUES (1)")
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("cbWithTx err = %v, want %v", err, want)
	}
	var n int
	if err := d.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("row count = %d, want 0 (fn-error should roll back)", n)
	}
}

func TestCBDB_PingForwardsClosed(t *testing.T) {
	t.Parallel()
	d := openMemDB(t)
	w := wrapDB(d, newCB(3))
	if err := w.PingContext(context.Background()); err != nil {
		t.Fatalf("PingContext: %v", err)
	}
}

// TestCBDB_AppErrorPassesThrough confirms application sentinel errors
// (here ErrObjectNotFound) flow through Exec without tripping the
// breaker, matching the Postgres wrapper's policy.
func TestCBDB_AppErrorPassesThrough(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openMemDB(t)
	if _, err := d.ExecContext(ctx, "CREATE TABLE t(a INT NOT NULL)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	cb := newCB(1)
	w := wrapDB(d, cb)

	// Drive an app-sentinel error through PostCheck repeatedly. The
	// CB classifier (isDBErrorForTest) treats sentinel errors as
	// non-DB so the failure counter stays at zero.
	for range 5 {
		// Direct PostCheck use: ExecContext on a valid table cannot
		// be made to return a sentinel without faking the inner DB,
		// so exercise the classifier through PostCheck directly,
		// which is the same code path the wrapper invokes.
		if err := cb.PostCheck(core.ErrObjectNotFound); !errors.Is(err, core.ErrObjectNotFound) {
			t.Fatalf("PostCheck err = %v, want ErrObjectNotFound", err)
		}
	}
	// Breaker should still be closed: a real DB call must succeed.
	if _, err := w.ExecContext(ctx, "INSERT INTO t(a) VALUES (1)"); err != nil {
		t.Fatalf("ExecContext after sentinel storm: %v", err)
	}
}

// TestCBDB_QueryRowContext_OpenBreakerStillExecutes pins the documented
// divergence: an open breaker does NOT short-circuit QueryRowContext on
// SQLite. The Postgres wrapper short-circuits via errRow because
// pgx.Row is an interface; *sql.Row has no Scanner injection point.
func TestCBDB_QueryRowContext_OpenBreakerStillExecutes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openMemDB(t)
	if _, err := d.ExecContext(ctx, "CREATE TABLE t(a INT); INSERT INTO t(a) VALUES (1)"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	w := wrapDB(d, newCB(1))
	// Trip the breaker.
	_, _ = w.ExecContext(ctx, "this is not sql")

	// Despite the open breaker, QueryRowContext still runs and Scan
	// succeeds because the inner row carries a valid result.
	var got int
	if err := w.QueryRowContext(ctx, "SELECT a FROM t").Scan(&got); err != nil {
		t.Fatalf("Scan: %v (expected success — open breaker does not short-circuit QueryRow)", err)
	}
	if got != 1 {
		t.Errorf("got %d, want 1", got)
	}
}

// TestCBDB_QueryRowContext_ScanErrorDoesNotFeedBreaker pins the second
// half of the documented divergence: a Scan-time DB error does not
// reach PostCheck because the wrapper cannot intercept Scan on a
// concrete *sql.Row. The breaker stays closed after multiple Scan
// failures and a subsequent valid Exec still succeeds.
func TestCBDB_QueryRowContext_ScanErrorDoesNotFeedBreaker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openMemDB(t)
	if _, err := d.ExecContext(ctx, "CREATE TABLE t(a INT)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	// Threshold = 2 so a single Scan-time error would trip if it
	// were counted. We feed two Scan-time errors and then prove the
	// breaker is still closed.
	w := wrapDB(d, newCB(2))

	for range 2 {
		var got int
		// No rows -> Scan returns sql.ErrNoRows, which the
		// classifier treats as a DB error. If this fed PostCheck
		// the breaker would trip.
		err := w.QueryRowContext(ctx, "SELECT a FROM t WHERE a = 999").Scan(&got)
		if err == nil {
			t.Fatal("expected sql.ErrNoRows or similar on empty result")
		}
	}
	// Breaker should still be closed -> a normal Exec succeeds.
	if _, err := w.ExecContext(ctx, "INSERT INTO t(a) VALUES (1)"); err != nil {
		t.Fatalf("Exec after Scan-error storm: %v (breaker tripped unexpectedly)", err)
	}
}

// TestCBWithTx_BeginTxFailureFeedsBreaker forces a BeginTx failure by
// closing the underlying *sql.DB before invoking cbWithTx. The breaker
// must observe the failure: a second cbWithTx call should short-circuit
// with ErrDBUnavailable instead of running fn.
func TestCBWithTx_BeginTxFailureFeedsBreaker(t *testing.T) {
	t.Parallel()
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if cerr := d.Close(); cerr != nil {
		t.Fatalf("close before test: %v", cerr)
	}
	cb := newCB(1)

	// First call: BeginTx fails because the DB is closed. PostCheck
	// trips the breaker.
	err = cbWithTx(context.Background(), d, cb, func(_ *sql.Tx) error { return nil })
	if err == nil {
		t.Fatal("expected BeginTx failure on closed DB")
	}

	// Second call: PreCheck should now short-circuit with the
	// sentinel.
	called := false
	err = cbWithTx(context.Background(), d, cb, func(_ *sql.Tx) error {
		called = true
		return nil
	})
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("err = %v, want ErrDBUnavailable", err)
	}
	if called {
		t.Fatal("fn ran while breaker was open")
	}
}

// TestCBWithTx_CommitFailureFeedsBreaker drives a real Commit-time
// failure via a deferred foreign-key constraint and asserts the breaker
// trips. Before the fix, Commit failures returned directly and the
// breaker never saw them, leaving the service blind to commit-time DB
// outages (lock contention, disk full, I/O errors). Deferred FKs are
// the simplest way in SQLite to force an error that surfaces at COMMIT
// rather than at the offending INSERT.
func TestCBWithTx_CommitFailureFeedsBreaker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openMemDB(t)

	// Enable FK enforcement and create parent + child tables with a
	// DEFERRABLE INITIALLY DEFERRED reference so the constraint is
	// only checked at COMMIT.
	if _, err := d.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable FKs: %v", err)
	}
	if _, err := d.ExecContext(ctx, `
		CREATE TABLE parent(id INTEGER PRIMARY KEY);
		CREATE TABLE child(pid INTEGER REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED);
	`); err != nil {
		t.Fatalf("schema setup: %v", err)
	}
	cb := newCB(1)

	// Insert a child row referencing a nonexistent parent. The
	// INSERT succeeds inside the txn; COMMIT fails because the
	// deferred FK constraint is checked at commit time.
	commitErr := cbWithTx(ctx, d, cb, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO child(pid) VALUES (999)`)
		return err
	})
	if commitErr == nil {
		t.Fatal("expected deferred-FK Commit failure")
	}

	// Breaker must now be open so a second cbWithTx call short-
	// circuits via PreCheck. This is the regression guard for the
	// commit-routing fix.
	called := false
	err := cbWithTx(ctx, d, cb, func(_ *sql.Tx) error {
		called = true
		return nil
	})
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("err = %v, want ErrDBUnavailable (breaker should trip on Commit failure)", err)
	}
	if called {
		t.Fatal("fn ran while breaker was open after Commit failure")
	}
}

// Compile-time check that cbDB satisfies the local dbAPI interface.
var _ dbAPI = (*cbDB)(nil)

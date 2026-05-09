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

// Compile-time check that cbDB satisfies the local dbAPI interface.
var _ dbAPI = (*cbDB)(nil)

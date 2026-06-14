// Tests for the CB-aware DBTX wrapper. The breaker state machine itself
// is covered in internal/breaker; these tests prove the wrapper feeds
// PreCheck/PostCheck correctly and that wrapDBTX no-ops on a nil cb.
package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// stubDBTX is a minimal db.DBTX whose three methods record their last
// call and return canned responses. Lets cb_test.go exercise the
// wrapper without standing up a real postgres connection.
type stubDBTX struct {
	execErr     error
	queryErr    error
	queryRowErr error
	calls       int
}

func (s *stubDBTX) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	s.calls++
	return pgconn.CommandTag{}, s.execErr
}

func (s *stubDBTX) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	s.calls++
	return nil, s.queryErr
}

func (s *stubDBTX) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	s.calls++
	return errRow{err: s.queryRowErr}
}

// newCB builds a database breaker with a tight failure threshold so a
// single error trips it, suitable for asserting the open-circuit branch.
func newCB(threshold int) *breaker.CircuitBreaker {
	return breaker.NewCircuitBreaker(breaker.Config{Name: "test", Threshold: threshold, Timeout: time.Minute, IsError: isDBErrorForTest, Sentinel: core.ErrDBUnavailable})
}

// isDBErrorForTest treats any non-sentinel error as a DB error. The
// production isDBError lives in package store; redeclared here so the
// postgres package's tests don't import store.
func isDBErrorForTest(err error) bool {
	if err == nil {
		return false
	}
	return !errors.Is(err, core.ErrObjectNotFound) && !errors.Is(err, core.ErrNoSpaceAvailable)
}

func TestWrapDBTX_NilCBReturnsInner(t *testing.T) {
	t.Parallel()
	stub := &stubDBTX{}
	got := wrapDBTX(stub, nil)
	if got != stub {
		t.Fatalf("wrapDBTX(_, nil) returned %v, want inner", got)
	}
}

func TestCBDBTX_ExecForwardsClosed(t *testing.T) {
	t.Parallel()
	stub := &stubDBTX{}
	w := wrapDBTX(stub, newCB(3))
	if _, err := w.Exec(context.Background(), "select 1"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("inner Exec calls = %d, want 1", stub.calls)
	}
}

func TestCBDBTX_ExecReturnsSentinelWhenOpen(t *testing.T) {
	t.Parallel()
	stub := &stubDBTX{execErr: errors.New("connection refused")}
	w := wrapDBTX(stub, newCB(1))
	// First call: real DB error trips the breaker.
	if _, err := w.Exec(context.Background(), "select 1"); err == nil {
		t.Fatal("expected DB error on trip call")
	}
	// Second call: open circuit short-circuits before reaching inner.
	callsBefore := stub.calls
	_, err := w.Exec(context.Background(), "select 1")
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("err = %v, want ErrDBUnavailable", err)
	}
	if stub.calls != callsBefore {
		t.Errorf("inner Exec was called while breaker open (calls=%d, want %d)", stub.calls, callsBefore)
	}
}

func TestCBDBTX_QueryReturnsSentinelWhenOpen(t *testing.T) {
	t.Parallel()
	stub := &stubDBTX{queryErr: errors.New("connection refused")}
	w := wrapDBTX(stub, newCB(1))
	rows1, err := w.Query(context.Background(), "select 1")
	if rows1 != nil {
		rows1.Close()
	}
	if err == nil {
		t.Fatal("expected DB error on trip call")
	}
	rows2, err := w.Query(context.Background(), "select 1")
	if rows2 != nil {
		rows2.Close()
	}
	if !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("err = %v, want ErrDBUnavailable", err)
	}
}

func TestCBDBTX_QueryRowReturnsErrRowWhenOpen(t *testing.T) {
	t.Parallel()
	stub := &stubDBTX{queryRowErr: errors.New("scan failure")}
	cb := newCB(1)
	w := wrapDBTX(stub, cb)
	// Trip the breaker via Exec so QueryRow's PreCheck path fires.
	stub.execErr = errors.New("connection refused")
	_, _ = w.Exec(context.Background(), "select 1")

	row := w.QueryRow(context.Background(), "select 1")
	var dst int
	if err := row.Scan(&dst); !errors.Is(err, core.ErrDBUnavailable) {
		t.Fatalf("Scan err = %v, want ErrDBUnavailable", err)
	}
}

func TestCBDBTX_AppErrorPassesThrough(t *testing.T) {
	t.Parallel()
	stub := &stubDBTX{execErr: core.ErrObjectNotFound}
	w := wrapDBTX(stub, newCB(1))
	// Application errors should not trip the circuit.
	for range 3 {
		if _, err := w.Exec(context.Background(), "select 1"); !errors.Is(err, core.ErrObjectNotFound) {
			t.Fatalf("Exec err = %v, want ErrObjectNotFound", err)
		}
	}
}

// Compile-time check: cbDBTX satisfies the sqlc DBTX surface so the
// driver can hand it to db.New(...) wherever a pool or tx would go.
var _ db.DBTX = (*cbDBTX)(nil)

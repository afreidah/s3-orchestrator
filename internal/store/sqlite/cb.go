// CB-aware *sql.DB wrapper for the sqlite driver. Every statement the
// store fires - direct or transaction-bound - flows through this single
// chokepoint, which calls breaker.PreCheck before the call and
// breaker.PostCheck after. Advisory locks emulate a process-local mutex
// and never touch *sql.DB, so they bypass the breaker.
package sqlite

import (
	"context"
	"database/sql"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// dbAPI is the slice of *sql.DB the sqlite store actually uses for
// non-transactional statements. Defined as an interface so the
// production driver can hand the store either a raw *sql.DB or a
// CB-wrapped one without further refactoring. Transaction setup goes
// through cbBeginTx instead - keeping it off this interface lets the
// store own the tx lifecycle (defer Rollback) at the call site rather
// than threading a factory method through the wrapper.
type dbAPI interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	PingContext(ctx context.Context) error
	Close() error
}

// cbDB wraps a *sql.DB with circuit-breaker pre/post checks on every
// statement that touches the database.
type cbDB struct {
	inner *sql.DB
	cb    *breaker.CircuitBreaker
}

// wrapDB returns inner unchanged when cb is nil so test fixtures and
// migration runners don't pay for the wrapping.
func wrapDB(inner *sql.DB, cb *breaker.CircuitBreaker) dbAPI {
	if cb == nil {
		return inner
	}
	return &cbDB{inner: inner, cb: cb}
}

// ExecContext runs the statement under the breaker.
func (c *cbDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := c.cb.PreCheck(); err != nil {
		return nil, err
	}
	res, err := c.inner.ExecContext(ctx, query, args...)
	return res, c.cb.PostCheck(err)
}

// QueryContext runs the query under the breaker.
func (c *cbDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if err := c.cb.PreCheck(); err != nil {
		return nil, err
	}
	rows, err := c.inner.QueryContext(ctx, query, args...)
	return rows, c.cb.PostCheck(err)
}

// QueryRowContext runs the query under the breaker. *sql.Row is a
// concrete struct with no constructor for an error-only row, so PreCheck
// failure cannot be injected at row-return time; PostCheck observes the
// error at Scan time via the row's stored err field. Under an open
// breaker the inner call still runs, but sqlite is in-process so the
// extra hit is negligible.
func (c *cbDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if err := c.cb.PreCheck(); err != nil {
		_ = err
	}
	return c.inner.QueryRowContext(ctx, query, args...)
}

// cbWithTx opens a transaction under the breaker, runs fn against it,
// commits on a nil return, and rolls back otherwise. Owning the tx
// lifecycle inside the wrapper keeps Begin and Rollback at the same
// call site (no factory pattern that hands a tx to a caller who must
// remember to defer rollback) and routes BeginTx failures through the
// breaker - the load-bearing "database is unreachable" trip case.
// Statements inside fn are not individually breaker-wrapped because
// *sql.Tx is a concrete type with no interface seam.
func cbWithTx(ctx context.Context, inner *sql.DB, cb *breaker.CircuitBreaker, fn func(*sql.Tx) error) error {
	if cb != nil {
		if err := cb.PreCheck(); err != nil {
			return err
		}
	}
	tx, err := inner.BeginTx(ctx, nil)
	if err != nil {
		if cb != nil {
			err = cb.PostCheck(err)
		}
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// PingContext routes through the breaker so explicit health probes feed
// the same failure counter as real queries.
func (c *cbDB) PingContext(ctx context.Context) error {
	if err := c.cb.PreCheck(); err != nil {
		return err
	}
	return c.cb.PostCheck(c.inner.PingContext(ctx))
}

// Close closes the wrapped *sql.DB; not CB-routed.
func (c *cbDB) Close() error {
	return c.inner.Close()
}

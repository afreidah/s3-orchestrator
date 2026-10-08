// -------------------------------------------------------------------------------
// SQLite Circuit Breaker Chokepoint
//
// Author: Alex Freidah
//
// CB-aware *sql.DB wrapper for the sqlite driver. Every statement the store
// fires - direct or transaction-bound - flows through this single chokepoint,
// which calls breaker.PreCheck before the call and breaker.PostCheck after.
// Advisory locks emulate a process-local mutex and never touch *sql.DB, so they
// bypass the breaker.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// dbAPI is the subset of *sql.DB the sqlite store uses for non-transactional
// statements, satisfied by a raw *sql.DB or the breaker-wrapped cbDB.
// Transactions go through cbWithTx instead.
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

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// wrapDB returns inner unchanged when cb is nil so test fixtures and
// migration runners don't pay for the wrapping.
func wrapDB(inner *sql.DB, cb *breaker.CircuitBreaker) dbAPI {
	if cb == nil {
		return inner
	}
	return &cbDB{inner: inner, cb: cb}
}

// -------------------------------------------------------------------------
// GUARDED STATEMENTS
// -------------------------------------------------------------------------

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

// QueryRowContext bypasses the breaker, unlike the Postgres wrapper: *sql.Row
// is a concrete type, so it cannot carry a PreCheck error or report Scan
// errors. An open breaker does not short-circuit it, and Scan failures are not
// counted.
func (c *cbDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return c.inner.QueryRowContext(ctx, query, args...)
}

// -------------------------------------------------------------------------
// TRANSACTIONS AND LIFECYCLE
// -------------------------------------------------------------------------

// cbWithTx opens a transaction under the breaker, runs fn against it, commits
// on a nil return, and rolls back otherwise. BeginTx and Commit failures feed
// the breaker, since I/O errors, full disks, and lock contention often surface
// only at commit. Statements inside fn are not breaker-wrapped because *sql.Tx
// is a concrete type; fn's errors are returned verbatim.
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
	err = tx.Commit()
	if cb != nil {
		return cb.PostCheck(err)
	}
	return err
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

// CB-aware sqlc DBTX wrapper for the postgres driver. Every sqlc query -
// pool-bound or tx-bound - flows through this single chokepoint, which
// calls breaker.PreCheck before the SQL and breaker.PostCheck after.
// Advisory locks bypass the breaker by going through pool.Acquire()
// directly (see advisory_lock.go).
package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// cbDBTX wraps any sqlc DBTX (pool, conn, or tx) with circuit-breaker
// pre/post checks.
type cbDBTX struct {
	inner db.DBTX
	cb    *breaker.CircuitBreaker
}

// wrapDBTX returns inner unchanged when cb is nil so test fixtures and
// non-CB callers don't pay for the wrapping.
func wrapDBTX(inner db.DBTX, cb *breaker.CircuitBreaker) db.DBTX {
	if cb == nil {
		return inner
	}
	return &cbDBTX{inner: inner, cb: cb}
}

// Exec runs the query under the breaker. PreCheck failure short-circuits
// before the inner Exec is invoked; PostCheck filters DB-shaped errors
// from app-shaped ones (sentinel pass-through, real DB faults trip the
// breaker).
func (c *cbDBTX) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := c.cb.PreCheck(); err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := c.inner.Exec(ctx, sql, args...)
	return tag, c.cb.PostCheck(err)
}

// Query runs the query under the breaker.
func (c *cbDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := c.cb.PreCheck(); err != nil {
		return nil, err
	}
	rows, err := c.inner.Query(ctx, sql, args...)
	return rows, c.cb.PostCheck(err)
}

// QueryRow runs the query under the breaker. PreCheck failure surfaces
// at Scan time via cbRow because pgx.Row has no separate error channel.
func (c *cbDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := c.cb.PreCheck(); err != nil {
		return errRow{err: err}
	}
	return cbRow{inner: c.inner.QueryRow(ctx, sql, args...), cb: c.cb}
}

// cbRow runs the inner Scan under the breaker's PostCheck so a DB
// error surfaced at Scan time still feeds the breaker's failure
// counter. Mirrors how the old cb_*.go decorators handled QueryRow.
type cbRow struct {
	inner pgx.Row
	cb    *breaker.CircuitBreaker
}

// Scan delegates to the wrapped row and feeds the result through PostCheck.
func (r cbRow) Scan(dest ...any) error {
	return r.cb.PostCheck(r.inner.Scan(dest...))
}

// errRow is a pgx.Row stub that returns a fixed error from Scan. Used to
// surface PreCheck failure (breaker open) at the caller's Scan call site.
type errRow struct{ err error }

// Scan returns the captured PreCheck error.
func (r errRow) Scan(_ ...any) error { return r.err }

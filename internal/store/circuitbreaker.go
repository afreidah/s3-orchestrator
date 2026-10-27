// -------------------------------------------------------------------------------
// Database Circuit Breaker Factory
//
// Author: Alex Freidah
//
// Builds the database circuit breaker, the error filter the driver-level
// DBTX/DB wrappers apply, and the recovery that closes it again. The breaker
// counts only failures that say the database cannot serve, and recovers
// through a ping the watchdog runs, never through a client request.
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// NewDatabaseBreaker returns the circuit breaker used to guard metadata store
// calls. requestError is the driver's test for an error the database returned
// while staying able to serve, which does not count. Wires the telemetry hook
// so CircuitBreakerState / CircuitBreakerTransitionsTotal are populated.
func NewDatabaseBreaker(cfg config.CircuitBreakerConfig, requestError func(error) bool) *breaker.CircuitBreaker {
	cb := breaker.NewCircuitBreaker(breaker.Config{
		Name:      "database",
		Threshold: cfg.FailureThreshold,
		Timeout:   cfg.OpenTimeout,
		IsError:   func(err error) bool { return unavailable(err, requestError) },
		Sentinel:  core.ErrDBUnavailable,
	})
	cb.SetOnStateChange(telemetry.NewDatabaseBreakerHook("database"))
	return cb
}

// DatabaseRecovery is the recovery for the database breaker: ping, which must
// bypass the breaker, every open timeout until the database answers. A ping
// costs nothing, so the checks do not back off.
func DatabaseRecovery(ping func(context.Context) error) breaker.Recovery {
	return breaker.Recovery{Check: ping}
}

// unavailable reports whether err says the database cannot serve. A cancelled
// or expired context is the caller giving up, the no-rows sentinels and the
// application signals (S3Error, ErrNoSpaceAvailable) are answers, and
// requestError covers the database refusing one request: none of them count.
// Anything else, a broken connection above all, does. INSERT ... ON CONFLICT
// DO NOTHING RETURNING reports ErrNoRows whenever the row already existed,
// which is an idempotent success.
func unavailable(err error, requestError func(error) bool) bool {
	switch {
	case err == nil,
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, sql.ErrNoRows), errors.Is(err, pgx.ErrNoRows),
		errors.Is(err, core.ErrNoSpaceAvailable):
		return false
	}
	if _, ok := errors.AsType[*core.S3Error](err); ok {
		return false
	}
	return !requestError(err)
}

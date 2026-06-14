// Database circuit breaker factory plus the error filter used by the
// driver-level DBTX/DB wrappers. The CB instance is a first-class
// DI-registered value; consumers that need lifecycle controls
// (IsHealthy, ResetStaleProbe) invoke *breaker.CircuitBreaker directly.
package store

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// NewDatabaseBreaker returns the circuit breaker used to guard metadata
// store calls. It treats S3Error, ErrNoSpaceAvailable, and "no rows"
// sentinels as application-level signals that should not trip the
// circuit. Wires the telemetry hook so CircuitBreakerState /
// CircuitBreakerTransitionsTotal are populated.
func NewDatabaseBreaker(cfg config.CircuitBreakerConfig) *breaker.CircuitBreaker {
	cb := breaker.NewCircuitBreaker(breaker.Config{
		Name:      "database",
		Threshold: cfg.FailureThreshold,
		Timeout:   cfg.OpenTimeout,
		IsError:   isDBError,
		Sentinel:  core.ErrDBUnavailable,
	})
	cb.SetOnStateChange(telemetry.NewDatabaseBreakerHook("database"))
	return cb
}

// isDBError returns true for genuine database failures. Application-
// level signals (S3Error, ErrNoSpaceAvailable) and the "no rows"
// sentinels (sql.ErrNoRows, pgx.ErrNoRows) do not trip the circuit.
// The no-rows filter matters now that the breaker lives at the driver
// chokepoint: queries like INSERT ... ON CONFLICT DO NOTHING RETURNING
// surface ErrNoRows via Scan whenever the row already existed, and
// those are normal idempotent successes - not DB faults.
func isDBError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*core.S3Error](err); ok {
		return false
	}
	if errors.Is(err, core.ErrNoSpaceAvailable) {
		return false
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	return true
}

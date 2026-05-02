// -------------------------------------------------------------------------------
// Database Circuit Breaker — helpers
//
// Author: Alex Freidah
//
// Houses the DB-specific error filter and the factory that constructs the
// shared *breaker.CircuitBreaker instance used by every per-role CB wrapper
// (cb_object.go, cb_quota.go, ...). The CB instance itself is a first-class
// DI-registered value; consumers that need lifecycle controls (IsHealthy,
// ResetStaleProbe) invoke *breaker.CircuitBreaker directly, not a store
// wrapper.
// -------------------------------------------------------------------------------

package store

import (
	"errors"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// NewDatabaseBreaker returns the circuit breaker used to guard metadata
// store calls. It treats S3Error and ErrNoSpaceAvailable as application-level
// failures that should not trip the circuit. Wires the telemetry hook so
// CircuitBreakerState / CircuitBreakerTransitionsTotal are populated.
func NewDatabaseBreaker(cfg config.CircuitBreakerConfig) *breaker.CircuitBreaker {
	cb := breaker.NewCircuitBreaker("database", cfg.FailureThreshold, cfg.OpenTimeout, isDBError, core.ErrDBUnavailable)
	cb.SetOnStateChange(telemetry.NewCircuitBreakerHook("database"))
	return cb
}

// isDBError returns true for genuine database failures. Application-level
// errors (S3Error, ErrNoSpaceAvailable) do not trip the circuit breaker.
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
	return true
}

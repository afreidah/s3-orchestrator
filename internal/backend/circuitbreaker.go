// -------------------------------------------------------------------------------
// CircuitBreakerBackend - Per-Backend Fault Isolation Wrapper
//
// Author: Alex Freidah
//
// Wraps an ObjectBackend with a circuit breaker so that a backend with expired
// credentials or a down provider is automatically excluded from request routing
// after consecutive failures. When the open timeout elapses, a single probe
// request tests recovery.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"io"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// CircuitBreakerBackend wraps an ObjectBackend with circuit breaker protection.
// All S3 operations are guarded: when the circuit is open, calls immediately
// return ErrBackendUnavailable without touching the real backend.
type CircuitBreakerBackend struct {
	real ObjectBackend
	*breaker.CircuitBreaker
}

// Compile-time check.
var _ ObjectBackend = (*CircuitBreakerBackend)(nil)

// NewCircuitBreakerBackend wraps a backend with per-backend circuit breaker logic.
func NewCircuitBreakerBackend(real ObjectBackend, name string, threshold int, timeout time.Duration) *CircuitBreakerBackend {
	return &CircuitBreakerBackend{
		real:           real,
		CircuitBreaker: breaker.NewCircuitBreaker(name, threshold, timeout, isBackendError, breaker.ErrBackendUnavailable),
	}
}

// Unwrap returns the underlying ObjectBackend. This is needed for code that
// type-asserts to a concrete type (e.g. SyncBackend asserting *S3Backend).
func (cb *CircuitBreakerBackend) Unwrap() ObjectBackend {
	return cb.real
}

// isBackendError returns true for all non-nil errors. Unlike the database
// circuit breaker, which exempts application-level errors (S3Error,
// ErrNoSpaceAvailable), backend errors are always genuine failures.
func isBackendError(err error) bool {
	return err != nil
}

// PutObject uploads an object to the backend with circuit breaker protection.
func (cb *CircuitBreakerBackend) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (string, error) {
		return cb.real.PutObject(ctx, key, body, size, contentType, metadata)
	})
}

// GetObject retrieves an object from the backend with circuit breaker protection.
func (cb *CircuitBreakerBackend) GetObject(ctx context.Context, key string, rangeHeader string) (*GetObjectResult, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (*GetObjectResult, error) {
		return cb.real.GetObject(ctx, key, rangeHeader)
	})
}

// HeadObject retrieves object metadata with circuit breaker protection.
func (cb *CircuitBreakerBackend) HeadObject(ctx context.Context, key string) (*HeadObjectResult, error) {
	return breaker.CBCall(cb.CircuitBreaker, func() (*HeadObjectResult, error) {
		return cb.real.HeadObject(ctx, key)
	})
}

// DeleteObject removes an object from the backend with circuit breaker protection.
func (cb *CircuitBreakerBackend) DeleteObject(ctx context.Context, key string) error {
	return breaker.CBCallNoResult(cb.CircuitBreaker, func() error {
		return cb.real.DeleteObject(ctx, key)
	})
}

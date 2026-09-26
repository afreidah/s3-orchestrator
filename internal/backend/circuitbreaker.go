// -------------------------------------------------------------------------------
// CircuitBreakerBackend - Per-Backend Fault Isolation Wrapper
//
// Author: Alex Freidah
//
// Wraps an ObjectBackend with a circuit breaker so that a backend with expired
// credentials or a down provider is automatically excluded from request routing
// after consecutive failures. Only failures that say something about the
// backend count: a network error, a 5xx, a 429, or a credential rejection
// (401/403). While open, every call is
// refused; recovery is tested out of band with a bucket health check (see
// RecoveryProber), never with a client request.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// CircuitBreakerBackend wraps an ObjectBackend with circuit breaker protection.
// All S3 operations are guarded: when the circuit is open, calls immediately
// return ErrBackendUnavailable without touching the real backend.
type CircuitBreakerBackend struct {
	real CheckedBackend
	*breaker.CircuitBreaker
}

// Compile-time check.
var _ ObjectBackend = (*CircuitBreakerBackend)(nil)

// CircuitBreakerConfig configures the circuit breaker wrapping a backend.
type CircuitBreakerConfig struct {
	Name      string
	Threshold int
	Timeout   time.Duration
}

// NewCircuitBreakerBackend wraps a backend with per-backend circuit breaker
// logic. The breaker is wired to the telemetry hook so transitions surface
// on the standard CircuitBreaker* metrics and the BackendCircuit*
// notification events. It recovers only through Recover, which a
// RecoveryProber calls once CheckHealth passes.
func NewCircuitBreakerBackend(real CheckedBackend, cfg CircuitBreakerConfig) *CircuitBreakerBackend {
	cb := breaker.NewCircuitBreaker(breaker.Config{
		Name:             cfg.Name,
		Threshold:        cfg.Threshold,
		Timeout:          cfg.Timeout,
		IsError:          isBackendError,
		Sentinel:         breaker.ErrBackendUnavailable,
		ExternalRecovery: true,
	})
	cb.SetOnStateChange(telemetry.NewCircuitBreakerHook(cfg.Name))
	return &CircuitBreakerBackend{
		real:           real,
		CircuitBreaker: cb,
	}
}

// Unwrap returns the underlying ObjectBackend. This is needed for code that
// type-asserts to a concrete type or to a narrow interface (e.g. the
// reconciler's objectLister, which extends ObjectBackend with ListObjects).
func (cb *CircuitBreakerBackend) Unwrap() ObjectBackend {
	return cb.real
}

// CheckHealth runs the wrapped backend's bucket health check directly,
// bypassing the breaker, so it can run while the circuit is open.
func (cb *CircuitBreakerBackend) CheckHealth(ctx context.Context) error {
	return cb.real.HeadBucket(ctx)
}

// isBackendError returns true for errors that indicate backend health issues:
// a failure with no HTTP status (connection refused, DNS, TLS, reset), a 5xx,
// a 429, or a 401/403 (expired or revoked credentials fail every request).
// Any other status is an answer from a working backend about one request - a
// missing key, a bad range, a failed precondition - and counting it would let
// a single bad object trip a healthy backend. Context cancellation and
// deadline are excluded too: they signal a caller-side timeout or shutdown,
// not the wrapped backend's health.
func isBackendError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	respErr, ok := errors.AsType[httpStatusError](err)
	if !ok {
		return true
	}
	switch status := respErr.HTTPStatusCode(); {
	case status >= http.StatusInternalServerError:
		return true
	case status == http.StatusTooManyRequests, status == http.StatusUnauthorized, status == http.StatusForbidden:
		return true
	default:
		return false
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// PutObject uploads an object to the backend with circuit breaker protection.
func (cb *CircuitBreakerBackend) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error) {
	return cb.Call(func() (string, error) {
		return cb.real.PutObject(ctx, key, body, size, contentType, metadata)
	})
}

// GetObject retrieves an object from the backend with circuit breaker protection.
func (cb *CircuitBreakerBackend) GetObject(ctx context.Context, key string, rangeHeader string) (*GetObjectResult, error) {
	return cb.Call(func() (*GetObjectResult, error) {
		return cb.real.GetObject(ctx, key, rangeHeader)
	})
}

// HeadObject retrieves object metadata with circuit breaker protection.
func (cb *CircuitBreakerBackend) HeadObject(ctx context.Context, key string) (*HeadObjectResult, error) {
	return cb.Call(func() (*HeadObjectResult, error) {
		return cb.real.HeadObject(ctx, key)
	})
}

// DeleteObject removes an object from the backend with circuit breaker protection.
func (cb *CircuitBreakerBackend) DeleteObject(ctx context.Context, key string) error {
	return cb.CallNoResult(func() error {
		return cb.real.DeleteObject(ctx, key)
	})
}

// CopyObject forwards a server-side copy through the circuit breaker
// when the wrapped backend implements Copier. When it does not,
// returns ErrCopyNotSupported so the caller falls back to materialized
// copy. CopyObject failures count toward the same breaker as other
// operations so a misbehaving backend's native copy path trips the
// breaker just like its PutObject/GetObject path.
func (cb *CircuitBreakerBackend) CopyObject(ctx context.Context, srcKey, dstKey, contentType string, metadata map[string]string) (string, error) {
	copier, ok := cb.real.(Copier)
	if !ok {
		return "", ErrCopyNotSupported
	}
	return cb.Call(func() (string, error) {
		return copier.CopyObject(ctx, srcKey, dstKey, contentType, metadata)
	})
}

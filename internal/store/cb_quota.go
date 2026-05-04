// -------------------------------------------------------------------------------
// CB Decorator  -  QuotaStore
//
// Author: Alex Freidah
//
// Wraps a core.QuotaStore (per-backend bytes_used + bytes_limit + the
// least-utilized / has-space lookups) so every call routes through the
// database CircuitBreaker. Returns ErrDBUnavailable instantly when the
// breaker is open so the write path can fail fast and trigger the
// degraded broadcast read path on the read side.
// -------------------------------------------------------------------------------

package store

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// cbQuotaStore wraps a QuotaStore with circuit-breaker protection.
type cbQuotaStore struct {
	inner core.QuotaStore
	cb    *breaker.CircuitBreaker
}

// NewCBQuotaStore returns a CB-protected view of inner typed as QuotaStore.
func NewCBQuotaStore(inner core.QuotaStore, cb *breaker.CircuitBreaker) core.QuotaStore {
	return &cbQuotaStore{inner: inner, cb: cb}
}

// GetBackendWithSpace forwards to the inner store under the breaker.
func (c *cbQuotaStore) GetBackendWithSpace(ctx context.Context, size int64, backendOrder []string) (string, error) {
	return breaker.CBCall(c.cb, func() (string, error) { return c.inner.GetBackendWithSpace(ctx, size, backendOrder) })
}

// GetLeastUtilizedBackend forwards to the inner store under the breaker.
func (c *cbQuotaStore) GetLeastUtilizedBackend(ctx context.Context, size int64, eligible []string) (string, error) {
	return breaker.CBCall(c.cb, func() (string, error) { return c.inner.GetLeastUtilizedBackend(ctx, size, eligible) })
}

// GetQuotaStats forwards to the inner store under the breaker.
func (c *cbQuotaStore) GetQuotaStats(ctx context.Context) (map[string]core.QuotaStat, error) {
	return breaker.CBCall(c.cb, func() (map[string]core.QuotaStat, error) { return c.inner.GetQuotaStats(ctx) })
}

// -------------------------------------------------------------------------------
// CB Decorator — QuotaStore
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// cbQuotaStore wraps a QuotaStore with circuit-breaker protection.
type cbQuotaStore struct {
	inner QuotaStore
	cb    *breaker.CircuitBreaker
}

// NewCBQuotaStore returns a CB-protected view of inner typed as QuotaStore.
func NewCBQuotaStore(inner QuotaStore, cb *breaker.CircuitBreaker) QuotaStore {
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
func (c *cbQuotaStore) GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error) {
	return breaker.CBCall(c.cb, func() (map[string]QuotaStat, error) { return c.inner.GetQuotaStats(ctx) })
}
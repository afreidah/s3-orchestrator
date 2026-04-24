// -------------------------------------------------------------------------------
// CB Decorator — LifecycleStore
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// cbLifecycleStore wraps a LifecycleStore with circuit-breaker protection.
type cbLifecycleStore struct {
	inner LifecycleStore
	cb    *breaker.CircuitBreaker
}

// NewCBLifecycleStore returns a CB-protected view typed as LifecycleStore.
func NewCBLifecycleStore(inner LifecycleStore, cb *breaker.CircuitBreaker) LifecycleStore {
	return &cbLifecycleStore{inner: inner, cb: cb}
}

// ListExpiredObjects forwards to the inner store under the breaker.
func (c *cbLifecycleStore) ListExpiredObjects(ctx context.Context, prefix string, cutoff time.Time, limit int) ([]ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]ObjectLocation, error) { return c.inner.ListExpiredObjects(ctx, prefix, cutoff, limit) })
}
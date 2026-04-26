// -------------------------------------------------------------------------------
// CB Decorator — ExpiredObjectsLister
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// cbExpiredObjectsLister wraps a ExpiredObjectsLister with circuit-breaker protection.
type cbExpiredObjectsLister struct {
	inner ExpiredObjectsLister
	cb    *breaker.CircuitBreaker
}

// NewCBExpiredObjectsLister returns a CB-protected view typed as ExpiredObjectsLister.
func NewCBExpiredObjectsLister(inner ExpiredObjectsLister, cb *breaker.CircuitBreaker) ExpiredObjectsLister {
	return &cbExpiredObjectsLister{inner: inner, cb: cb}
}

// ListExpiredObjects forwards to the inner store under the breaker.
func (c *cbExpiredObjectsLister) ListExpiredObjects(ctx context.Context, prefix string, cutoff time.Time, limit int) ([]ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]ObjectLocation, error) { return c.inner.ListExpiredObjects(ctx, prefix, cutoff, limit) })
}
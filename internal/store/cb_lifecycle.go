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
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// cbExpiredObjectsLister wraps a ExpiredObjectsLister with circuit-breaker protection.
type cbExpiredObjectsLister struct {
	inner core.ExpiredObjectsLister
	cb    *breaker.CircuitBreaker
}

// NewCBExpiredObjectsLister returns a CB-protected view typed as ExpiredObjectsLister.
func NewCBExpiredObjectsLister(inner core.ExpiredObjectsLister, cb *breaker.CircuitBreaker) core.ExpiredObjectsLister {
	return &cbExpiredObjectsLister{inner: inner, cb: cb}
}

// ListExpiredObjects forwards to the inner store under the breaker.
func (c *cbExpiredObjectsLister) ListExpiredObjects(ctx context.Context, prefix string, cutoff time.Time, limit int) ([]core.ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]core.ObjectLocation, error) { return c.inner.ListExpiredObjects(ctx, prefix, cutoff, limit) })
}

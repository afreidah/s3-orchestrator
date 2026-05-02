// -------------------------------------------------------------------------------
// CB Decorator — UsageFlusher
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// cbUsageFlusher wraps a UsageFlusher with circuit-breaker protection.
type cbUsageFlusher struct {
	inner core.UsageFlusher
	cb    *breaker.CircuitBreaker
}

// NewCBUsageFlusher returns a CB-protected view typed as UsageFlusher.
func NewCBUsageFlusher(inner core.UsageFlusher, cb *breaker.CircuitBreaker) core.UsageFlusher {
	return &cbUsageFlusher{inner: inner, cb: cb}
}

// FlushUsageDeltas forwards to the inner store under the breaker.
func (c *cbUsageFlusher) FlushUsageDeltas(ctx context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error {
	return breaker.CBCallNoResult(c.cb, func() error {
		return c.inner.FlushUsageDeltas(ctx, backendName, period, apiRequests, egressBytes, ingressBytes)
	})
}

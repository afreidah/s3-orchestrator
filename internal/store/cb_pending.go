// -------------------------------------------------------------------------------
// CB Decorator - PendingStore
//
// Author: Alex Freidah
//
// Forwards every PendingStore method through the shared database circuit
// breaker so a degraded Postgres instance does not block reaper ticks or
// write-path intent inserts indefinitely.
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// cbPendingStore wraps a PendingStore with circuit-breaker protection.
type cbPendingStore struct {
	inner core.PendingStore
	cb    *breaker.CircuitBreaker
}

// NewCBPendingStore returns a CB-protected view typed as PendingStore.
func NewCBPendingStore(inner core.PendingStore, cb *breaker.CircuitBreaker) core.PendingStore {
	return &cbPendingStore{inner: inner, cb: cb}
}

// InsertPending forwards to the inner store under the breaker.
func (c *cbPendingStore) InsertPending(ctx context.Context, p *core.PendingObject) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.InsertPending(ctx, p) })
}

// DeletePending forwards to the inner store under the breaker.
func (c *cbPendingStore) DeletePending(ctx context.Context, intentID string) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.DeletePending(ctx, intentID) })
}

// GetStalePending forwards to the inner store under the breaker.
func (c *cbPendingStore) GetStalePending(ctx context.Context, olderThan time.Time, limit int) ([]core.PendingObject, error) {
	return breaker.CBCall(c.cb, func() ([]core.PendingObject, error) {
		return c.inner.GetStalePending(ctx, olderThan, limit)
	})
}

// PromotePending forwards to the inner store under the breaker. The
// (PendingPromoteResult, []DeletedCopy) pair is returned via a tuple so
// the generic CBCall helper can handle the single-result signature.
func (c *cbPendingStore) PromotePending(ctx context.Context, p *core.PendingObject) (core.PendingPromoteResult, []core.DeletedCopy, error) {
	type promoteOut struct {
		result    core.PendingPromoteResult
		displaced []core.DeletedCopy
	}
	out, err := breaker.CBCall(c.cb, func() (promoteOut, error) {
		r, d, err := c.inner.PromotePending(ctx, p)
		return promoteOut{result: r, displaced: d}, err
	})
	return out.result, out.displaced, err
}

// PendingDepth forwards to the inner store under the breaker.
func (c *cbPendingStore) PendingDepth(ctx context.Context) (int64, error) {
	return breaker.CBCall(c.cb, func() (int64, error) { return c.inner.PendingDepth(ctx) })
}

// DeletePendingByBackend forwards to the inner store under the breaker.
func (c *cbPendingStore) DeletePendingByBackend(ctx context.Context, backendName string) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.DeletePendingByBackend(ctx, backendName) })
}

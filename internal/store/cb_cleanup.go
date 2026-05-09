// -------------------------------------------------------------------------------
// CB Decorator  -  CleanupStore
//
// Author: Alex Freidah
//
// Wraps a core.CleanupStore (cleanup_queue + cleanup_dlq + orphan_bytes
// counters) so every call routes through the database CircuitBreaker.
// When the breaker is open, calls return ErrDBUnavailable instantly;
// when closed, they delegate to the inner store. Decorator pattern
// keeps the cleanup worker unaware of the breaker.
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// cbCleanupStore wraps a CleanupStore with circuit-breaker protection.
type cbCleanupStore struct {
	inner core.CleanupStore
	cb    *breaker.CircuitBreaker
}

// NewCBCleanupStore returns a CB-protected view typed as CleanupStore.
func NewCBCleanupStore(inner core.CleanupStore, cb *breaker.CircuitBreaker) core.CleanupStore {
	return &cbCleanupStore{inner: inner, cb: cb}
}

// EnqueueCleanup forwards to the inner store under the breaker.
func (c *cbCleanupStore) EnqueueCleanup(ctx context.Context, backendName, objectKey, reason string, sizeBytes int64) error {
	return breaker.CBCallNoResult(c.cb, func() error {
		return c.inner.EnqueueCleanup(ctx, backendName, objectKey, reason, sizeBytes)
	})
}

// GetPendingCleanups forwards to the inner store under the breaker.
func (c *cbCleanupStore) GetPendingCleanups(ctx context.Context, limit int) ([]core.CleanupItem, error) {
	return breaker.CBCall(c.cb, func() ([]core.CleanupItem, error) { return c.inner.GetPendingCleanups(ctx, limit) })
}

// ClaimPendingCleanups forwards to the inner store under the breaker.
func (c *cbCleanupStore) ClaimPendingCleanups(ctx context.Context, limit int, instanceID string, graceCutoff time.Time) ([]core.CleanupItem, error) {
	return breaker.CBCall(c.cb, func() ([]core.CleanupItem, error) {
		return c.inner.ClaimPendingCleanups(ctx, limit, instanceID, graceCutoff)
	})
}

// CompleteCleanupItem forwards to the inner store under the breaker.
func (c *cbCleanupStore) CompleteCleanupItem(ctx context.Context, id int64) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.CompleteCleanupItem(ctx, id) })
}

// RetryCleanupItem forwards to the inner store under the breaker.
func (c *cbCleanupStore) RetryCleanupItem(ctx context.Context, id int64, backoff time.Duration, lastError string) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.RetryCleanupItem(ctx, id, backoff, lastError) })
}

// CleanupQueueDepth forwards to the inner store under the breaker.
func (c *cbCleanupStore) CleanupQueueDepth(ctx context.Context) (int64, error) {
	return breaker.CBCall(c.cb, func() (int64, error) { return c.inner.CleanupQueueDepth(ctx) })
}

// IncrementOrphanBytes forwards to the inner store under the breaker.
func (c *cbCleanupStore) IncrementOrphanBytes(ctx context.Context, backendName string, amount int64) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.IncrementOrphanBytes(ctx, backendName, amount) })
}

// DecrementOrphanBytes forwards to the inner store under the breaker.
func (c *cbCleanupStore) DecrementOrphanBytes(ctx context.Context, backendName string, amount int64) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.DecrementOrphanBytes(ctx, backendName, amount) })
}

// SweepStaleCleanupQueueRows forwards to the inner store under the breaker.
func (c *cbCleanupStore) SweepStaleCleanupQueueRows(ctx context.Context, key, backend string) (int64, error) {
	return breaker.CBCall(c.cb, func() (int64, error) {
		return c.inner.SweepStaleCleanupQueueRows(ctx, key, backend)
	})
}

// MoveCleanupToDLQ forwards to the inner store under the breaker.
func (c *cbCleanupStore) MoveCleanupToDLQ(ctx context.Context, id int64, lastError string) (bool, error) {
	return breaker.CBCall(c.cb, func() (bool, error) {
		return c.inner.MoveCleanupToDLQ(ctx, id, lastError)
	})
}

// CleanupDLQDepth forwards to the inner store under the breaker.
func (c *cbCleanupStore) CleanupDLQDepth(ctx context.Context) (int64, error) {
	return breaker.CBCall(c.cb, func() (int64, error) {
		return c.inner.CleanupDLQDepth(ctx)
	})
}

// -------------------------------------------------------------------------------
// CB Decorator — CleanupStore
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// cbCleanupStore wraps a CleanupStore with circuit-breaker protection.
type cbCleanupStore struct {
	inner CleanupStore
	cb    *breaker.CircuitBreaker
}

// NewCBCleanupStore returns a CB-protected view typed as CleanupStore.
func NewCBCleanupStore(inner CleanupStore, cb *breaker.CircuitBreaker) CleanupStore {
	return &cbCleanupStore{inner: inner, cb: cb}
}

// EnqueueCleanup forwards to the inner store under the breaker.
func (c *cbCleanupStore) EnqueueCleanup(ctx context.Context, backendName, objectKey, reason string, sizeBytes int64) error {
	return breaker.CBCallNoResult(c.cb, func() error {
		return c.inner.EnqueueCleanup(ctx, backendName, objectKey, reason, sizeBytes)
	})
}

// GetPendingCleanups forwards to the inner store under the breaker.
func (c *cbCleanupStore) GetPendingCleanups(ctx context.Context, limit int) ([]CleanupItem, error) {
	return breaker.CBCall(c.cb, func() ([]CleanupItem, error) { return c.inner.GetPendingCleanups(ctx, limit) })
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
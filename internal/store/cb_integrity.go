// -------------------------------------------------------------------------------
// CB Decorator  -  IntegrityStore
//
// Author: Alex Freidah
//
// Wraps a core.IntegrityStore (random-sample selection, hash storage,
// missing-hash listing) so every call routes through the database
// CircuitBreaker. Keeps the scrubber from spinning on an unreachable
// database by surfacing ErrDBUnavailable instantly when the breaker is
// open.
// -------------------------------------------------------------------------------

package store

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// cbIntegrityStore wraps an IntegrityStore with circuit-breaker protection.
type cbIntegrityStore struct {
	inner core.IntegrityStore
	cb    *breaker.CircuitBreaker
}

// NewCBIntegrityStore returns a CB-protected view typed as IntegrityStore.
func NewCBIntegrityStore(inner core.IntegrityStore, cb *breaker.CircuitBreaker) core.IntegrityStore {
	return &cbIntegrityStore{inner: inner, cb: cb}
}

// GetRandomHashedObjects forwards to the inner store under the breaker.
func (c *cbIntegrityStore) GetRandomHashedObjects(ctx context.Context, limit int) ([]core.ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]core.ObjectLocation, error) { return c.inner.GetRandomHashedObjects(ctx, limit) })
}

// GetObjectsWithoutHash forwards to the inner store under the breaker.
func (c *cbIntegrityStore) GetObjectsWithoutHash(ctx context.Context, limit, offset int) ([]core.ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]core.ObjectLocation, error) { return c.inner.GetObjectsWithoutHash(ctx, limit, offset) })
}

// UpdateContentHash forwards to the inner store under the breaker.
func (c *cbIntegrityStore) UpdateContentHash(ctx context.Context, key, backendName, hash string) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.UpdateContentHash(ctx, key, backendName, hash) })
}

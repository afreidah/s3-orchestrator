// -------------------------------------------------------------------------------
// CB Decorator — IntegrityStore
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// cbIntegrityStore wraps an IntegrityStore with circuit-breaker protection.
type cbIntegrityStore struct {
	inner IntegrityStore
	cb    *breaker.CircuitBreaker
}

// NewCBIntegrityStore returns a CB-protected view typed as IntegrityStore.
func NewCBIntegrityStore(inner IntegrityStore, cb *breaker.CircuitBreaker) IntegrityStore {
	return &cbIntegrityStore{inner: inner, cb: cb}
}

// GetRandomHashedObjects forwards to the inner store under the breaker.
func (c *cbIntegrityStore) GetRandomHashedObjects(ctx context.Context, limit int) ([]ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]ObjectLocation, error) { return c.inner.GetRandomHashedObjects(ctx, limit) })
}

// GetObjectsWithoutHash forwards to the inner store under the breaker.
func (c *cbIntegrityStore) GetObjectsWithoutHash(ctx context.Context, limit, offset int) ([]ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]ObjectLocation, error) { return c.inner.GetObjectsWithoutHash(ctx, limit, offset) })
}

// UpdateContentHash forwards to the inner store under the breaker.
func (c *cbIntegrityStore) UpdateContentHash(ctx context.Context, key, backendName, hash string) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.UpdateContentHash(ctx, key, backendName, hash) })
}
// -------------------------------------------------------------------------------
// CB Decorator — BackendLifecycleStore
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// cbBackendLifecycleStore wraps a BackendLifecycleStore with circuit-breaker
// protection.
type cbBackendLifecycleStore struct {
	inner BackendLifecycleStore
	cb    *breaker.CircuitBreaker
}

// NewCBBackendLifecycleStore returns a CB-protected view typed as
// BackendLifecycleStore.
func NewCBBackendLifecycleStore(inner BackendLifecycleStore, cb *breaker.CircuitBreaker) BackendLifecycleStore {
	return &cbBackendLifecycleStore{inner: inner, cb: cb}
}

// BackendObjectStats forwards to the inner store using manual Pre/PostCheck
// (the return shape does not compose with CBCall's generic signature).
func (c *cbBackendLifecycleStore) BackendObjectStats(ctx context.Context, backendName string) (int64, int64, error) {
	if err := c.cb.PreCheck(); err != nil {
		return 0, 0, err
	}
	count, bytes, err := c.inner.BackendObjectStats(ctx, backendName)
	return count, bytes, c.cb.PostCheck(err)
}

// DeleteBackendData forwards to the inner store under the breaker.
func (c *cbBackendLifecycleStore) DeleteBackendData(ctx context.Context, backendName string) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.DeleteBackendData(ctx, backendName) })
}
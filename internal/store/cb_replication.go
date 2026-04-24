// -------------------------------------------------------------------------------
// CB Decorator — ReplicationStore
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// cbReplicationStore wraps a ReplicationStore with circuit-breaker protection.
type cbReplicationStore struct {
	inner ReplicationStore
	cb    *breaker.CircuitBreaker
}

// NewCBReplicationStore returns a CB-protected view typed as ReplicationStore.
func NewCBReplicationStore(inner ReplicationStore, cb *breaker.CircuitBreaker) ReplicationStore {
	return &cbReplicationStore{inner: inner, cb: cb}
}

// GetUnderReplicatedObjects forwards to the inner store under the breaker.
func (c *cbReplicationStore) GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]ObjectLocation, error) { return c.inner.GetUnderReplicatedObjects(ctx, factor, limit) })
}

// GetUnderReplicatedObjectsExcluding forwards to the inner store under the breaker.
func (c *cbReplicationStore) GetUnderReplicatedObjectsExcluding(ctx context.Context, factor, limit int, excludedBackends []string) ([]ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]ObjectLocation, error) {
		return c.inner.GetUnderReplicatedObjectsExcluding(ctx, factor, limit, excludedBackends)
	})
}

// RecordReplica forwards to the inner store under the breaker.
func (c *cbReplicationStore) RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string, size int64) (bool, error) {
	return breaker.CBCall(c.cb, func() (bool, error) { return c.inner.RecordReplica(ctx, key, targetBackend, sourceBackend, size) })
}

// GetOverReplicatedObjects forwards to the inner store under the breaker.
func (c *cbReplicationStore) GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]ObjectLocation, error) { return c.inner.GetOverReplicatedObjects(ctx, factor, limit) })
}

// CountOverReplicatedObjects forwards to the inner store under the breaker.
func (c *cbReplicationStore) CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error) {
	return breaker.CBCall(c.cb, func() (int64, error) { return c.inner.CountOverReplicatedObjects(ctx, factor) })
}

// RemoveExcessCopy forwards to the inner store under the breaker.
func (c *cbReplicationStore) RemoveExcessCopy(ctx context.Context, key, backendName string, size int64) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.RemoveExcessCopy(ctx, key, backendName, size) })
}
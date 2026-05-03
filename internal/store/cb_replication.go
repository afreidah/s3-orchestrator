// -------------------------------------------------------------------------------
// CB Decorator — ReplicationStore
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package store

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// cbReplicationStore wraps a ReplicationStore with circuit-breaker protection.
type cbReplicationStore struct {
	inner core.ReplicationStore
	cb    *breaker.CircuitBreaker
}

// NewCBReplicationStore returns a CB-protected view typed as ReplicationStore.
func NewCBReplicationStore(inner core.ReplicationStore, cb *breaker.CircuitBreaker) core.ReplicationStore {
	return &cbReplicationStore{inner: inner, cb: cb}
}

// GetUnderReplicatedObjects forwards to the inner store under the breaker.
func (c *cbReplicationStore) GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]core.ObjectLocation, error) { return c.inner.GetUnderReplicatedObjects(ctx, factor, limit) })
}

// GetUnderReplicatedObjectsExcluding forwards to the inner store under the breaker.
func (c *cbReplicationStore) GetUnderReplicatedObjectsExcluding(ctx context.Context, factor, limit int, excludedBackends []string) ([]core.ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]core.ObjectLocation, error) {
		return c.inner.GetUnderReplicatedObjectsExcluding(ctx, factor, limit, excludedBackends)
	})
}

// RecordReplica forwards to the inner store under the breaker. The
// (size, inserted) pair is returned via a tuple so the generic CBCall
// helper can handle the single-result signature.
func (c *cbReplicationStore) RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string) (int64, bool, error) {
	type recordOut struct {
		size     int64
		inserted bool
	}
	out, err := breaker.CBCall(c.cb, func() (recordOut, error) {
		size, inserted, err := c.inner.RecordReplica(ctx, key, targetBackend, sourceBackend)
		return recordOut{size: size, inserted: inserted}, err
	})
	return out.size, out.inserted, err
}

// GetOverReplicatedObjects forwards to the inner store under the breaker.
func (c *cbReplicationStore) GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]core.ObjectLocation, error) { return c.inner.GetOverReplicatedObjects(ctx, factor, limit) })
}

// CountOverReplicatedObjects forwards to the inner store under the breaker.
func (c *cbReplicationStore) CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error) {
	return breaker.CBCall(c.cb, func() (int64, error) { return c.inner.CountOverReplicatedObjects(ctx, factor) })
}

// RemoveExcessCopy forwards to the inner store under the breaker.
func (c *cbReplicationStore) RemoveExcessCopy(ctx context.Context, key, backendName string, size int64) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.RemoveExcessCopy(ctx, key, backendName, size) })
}

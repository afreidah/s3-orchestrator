// -------------------------------------------------------------------------------
// CB Decorator — ObjectStore
//
// Author: Alex Freidah
//
// Forwards ObjectStore calls through a shared *breaker.CircuitBreaker. When
// the circuit is open, methods return ErrDBUnavailable without reaching the
// inner store.
// -------------------------------------------------------------------------------

package store

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// cbObjectStore wraps an ObjectStore with circuit-breaker protection.
type cbObjectStore struct {
	inner core.ObjectStore
	cb    *breaker.CircuitBreaker
}

// NewCBObjectStore returns a CB-protected view of inner typed as ObjectStore.
func NewCBObjectStore(inner core.ObjectStore, cb *breaker.CircuitBreaker) core.ObjectStore {
	return &cbObjectStore{inner: inner, cb: cb}
}

// GetAllObjectLocations forwards to the inner store under the breaker.
func (c *cbObjectStore) GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]core.ObjectLocation, error) { return c.inner.GetAllObjectLocations(ctx, key) })
}

// GetObjectBackendsForKeys forwards to the inner store under the breaker.
func (c *cbObjectStore) GetObjectBackendsForKeys(ctx context.Context, keys []string) (map[string][]string, error) {
	return breaker.CBCall(c.cb, func() (map[string][]string, error) { return c.inner.GetObjectBackendsForKeys(ctx, keys) })
}

// RecordObject forwards to the inner store under the breaker.
func (c *cbObjectStore) RecordObject(ctx context.Context, key, backend string, size int64, enc *core.EncryptionMeta) ([]core.DeletedCopy, error) {
	return breaker.CBCall(c.cb, func() ([]core.DeletedCopy, error) { return c.inner.RecordObject(ctx, key, backend, size, enc) })
}

// RecordObjectAndClearPending forwards to the inner store under the breaker.
func (c *cbObjectStore) RecordObjectAndClearPending(ctx context.Context, key, backend string, size int64, enc *core.EncryptionMeta, intentID string) ([]core.DeletedCopy, error) {
	return breaker.CBCall(c.cb, func() ([]core.DeletedCopy, error) {
		return c.inner.RecordObjectAndClearPending(ctx, key, backend, size, enc, intentID)
	})
}

// DeleteObject forwards to the inner store under the breaker.
func (c *cbObjectStore) DeleteObject(ctx context.Context, key string) ([]core.DeletedCopy, error) {
	return breaker.CBCall(c.cb, func() ([]core.DeletedCopy, error) { return c.inner.DeleteObject(ctx, key) })
}

// DeleteObjectsBatch forwards the batch-delete to the inner store
// under the breaker.
func (c *cbObjectStore) DeleteObjectsBatch(ctx context.Context, keys []string) (map[string][]core.DeletedCopy, error) {
	return breaker.CBCall(c.cb, func() (map[string][]core.DeletedCopy, error) {
		return c.inner.DeleteObjectsBatch(ctx, keys)
	})
}

// ListObjects forwards to the inner store under the breaker.
func (c *cbObjectStore) ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.ListObjectsResult, error) {
	return breaker.CBCall(c.cb, func() (*core.ListObjectsResult, error) { return c.inner.ListObjects(ctx, prefix, startAfter, maxKeys) })
}

// ListObjectsByBackend forwards to the inner store under the breaker.
func (c *cbObjectStore) ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]core.ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]core.ObjectLocation, error) { return c.inner.ListObjectsByBackend(ctx, backendName, limit) })
}

// ListObjectsByBackendKeyAsc forwards the paginated key-asc listing under
// the breaker. Used by ReconcileBackend's bounded-memory sorted-merge join.
func (c *cbObjectStore) ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterKey string, limit int) ([]core.ObjectLocation, error) {
	return breaker.CBCall(c.cb, func() ([]core.ObjectLocation, error) {
		return c.inner.ListObjectsByBackendKeyAsc(ctx, backendName, afterKey, limit)
	})
}

// MoveObjectLocation forwards to the inner store under the breaker.
func (c *cbObjectStore) MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error) {
	return breaker.CBCall(c.cb, func() (int64, error) { return c.inner.MoveObjectLocation(ctx, key, fromBackend, toBackend) })
}

// ImportObject forwards to the inner store under the breaker.
func (c *cbObjectStore) ImportObject(ctx context.Context, key, backend string, size int64) (bool, error) {
	return breaker.CBCall(c.cb, func() (bool, error) { return c.inner.ImportObject(ctx, key, backend, size) })
}

// DeleteObjectLocation forwards to the inner store under the breaker.
func (c *cbObjectStore) DeleteObjectLocation(ctx context.Context, key, backendName string) error {
	return breaker.CBCallNoResult(c.cb, func() error { return c.inner.DeleteObjectLocation(ctx, key, backendName) })
}

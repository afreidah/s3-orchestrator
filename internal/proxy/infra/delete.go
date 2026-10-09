// -------------------------------------------------------------------------------
// Backend Runtime - Deletes
//
// Author: Alex Freidah
//
// Every backend delete goes through here, so the timeout and the API charge are
// applied in one place. DeleteMany removes many keys from one backend in as few
// requests as the backend allows: a batch where it supports one, otherwise
// single deletes in parallel. A backend that declines batches is remembered for
// the life of the process so it is not asked again.
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"errors"
	"sync"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// singleDeleteConcurrency caps the single deletes DeleteMany runs at once
// against one backend when it cannot batch them.
const singleDeleteConcurrency = 16

// batchDeleteRequestKeys is the most keys one batch request carries, which
// sets how many requests a batch is charged as.
const batchDeleteRequestKeys = 1000

// Delete removes the bytes at storageKey on the named backend under the
// configured backend timeout, and charges the DELETE whether or not it
// succeeds. storageKey is the path the bytes occupy, which for a per-write copy
// is not the object's key.
func (c *BackendRuntime) Delete(ctx context.Context, name string, be backend.ObjectBackend, storageKey string) error {
	dctx, dcancel := c.WithTimeout(ctx)
	defer dcancel()
	err := be.DeleteObject(dctx, storageKey)
	c.Acct().APICall(s3op.DeleteObject, name)
	return err
}

// DeleteMany removes storageKeys from the named backend and reports the keys it
// could not delete. A key absent from the result was deleted or was already
// gone.
func (c *BackendRuntime) DeleteMany(ctx context.Context, name string, be backend.ObjectBackend, storageKeys []string) map[string]error {
	if len(storageKeys) > 1 {
		if failed, ok := c.deleteBatch(ctx, name, be, storageKeys); ok {
			return failed
		}
	}
	return c.deleteEach(ctx, name, be, storageKeys)
}

// deleteBatch deletes storageKeys in batch requests, reporting false when the
// backend cannot batch so the caller deletes them singly. Each request is
// charged as one DeleteObjects call. A request that fails as a whole leaves
// every key failed.
func (c *BackendRuntime) deleteBatch(ctx context.Context, name string, be backend.ObjectBackend, storageKeys []string) (map[string]error, bool) {
	deleter, ok := be.(backend.BatchDeleter)
	if !ok {
		return nil, false
	}
	if _, declined := c.batchDeclined.Load(name); declined {
		return nil, false
	}

	dctx, dcancel := c.WithTimeout(ctx)
	defer dcancel()
	failed, err := deleter.DeleteObjects(dctx, storageKeys)
	if errors.Is(err, backend.ErrBatchDeleteNotSupported) {
		c.batchDeclined.Store(name, struct{}{})
		c.Log().InfoContext(ctx, "backend declined batch delete, deleting keys singly", "backend", name)
		return nil, false
	}
	for range (len(storageKeys) + batchDeleteRequestKeys - 1) / batchDeleteRequestKeys {
		c.Acct().APICall(s3op.DeleteObjects, name)
	}
	if err != nil {
		failed = make(map[string]error, len(storageKeys))
		for _, k := range storageKeys {
			failed[k] = err
		}
	}
	return failed, true
}

// deleteEach deletes storageKeys one request at a time, several at once.
func (c *BackendRuntime) deleteEach(ctx context.Context, name string, be backend.ObjectBackend, storageKeys []string) map[string]error {
	var mu sync.Mutex
	failed := make(map[string]error)
	workerpool.Run(ctx, singleDeleteConcurrency, storageKeys, func(ctx context.Context, key string) {
		if err := c.Delete(ctx, name, be, key); err != nil {
			mu.Lock()
			failed[key] = err
			mu.Unlock()
		}
	})
	return failed
}

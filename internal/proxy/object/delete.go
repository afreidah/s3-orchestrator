// -------------------------------------------------------------------------------
// Object Manager - DELETE and Batch DELETE
//
// Author: Alex Freidah
//
// DeleteObject single-key fanout (metadata delete in one tx, then a
// concurrent backend DELETE per copy) and DeleteObjects batch flattening
// + bounded-concurrency worker pool. Per-backend DELETE API-call
// accounting is owned by writepath.Coordinator.DeleteOrEnqueue;
// success-finalization helpers live in mutation_finalize.go.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// DeleteObject removes an object from the backend where it's stored.
func (o *Manager) DeleteObject(ctx context.Context, key string) error {
	const operation = s3op.DeleteObject
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation.String(),
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()

	copies, err := o.stores.DeleteObject(ctx, key)
	if err != nil {
		if errors.Is(err, core.ErrObjectNotFound) {
			// Object not in our tracking - treat as success (idempotent delete)
			span.SetStatus(codes.Ok, "object not found - treating as success")
			return nil
		}
		return o.core.ClassifyWriteError(span, operation.String(), err)
	}

	span.SetAttributes(attribute.Int("copies.deleted", len(copies)))

	// Drop the location cache entry up front so concurrent readers
	// during the backend fanout (which can take seconds) do not get
	// pointed at a backend that is in the middle of being deleted from.
	// The final invalidateObjectCaches below is a redundant no-op for
	// this key but keeps every mutation path ending with one helper.
	o.cache.Delete(key)

	workerpool.Run(ctx, len(copies), copies, func(ctx context.Context, cp core.DeletedCopy) {
		backend, ok := o.core.Backends()[cp.BackendName]
		if !ok {
			o.log.WarnContext(ctx, "backend not found for delete",
				"backend", cp.BackendName, "key", key)
			return
		}
		o.coord.DeleteOrEnqueue(ctx, backend, cp.BackendName, key, "delete_failed", cp.SizeBytes)
	})

	o.finalizeDelete(ctx, span, key, copies, start)
	return nil
}

// defaultBatchDeleteConcurrency caps how many per-key backend DELETE
// fanouts run at once inside DeleteObjects. Picked to absorb a typical
// S3 batch of 1000 keys without saturating any single backend's
// connection pool or burning API quota in a burst.
const defaultBatchDeleteConcurrency = 10

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// DeleteObjectResult holds the outcome of a single key within a batch delete.
type DeleteObjectResult struct {
	Key string `json:"key,omitempty"`
	Err error  `json:"err,omitempty"`
}

// batchDeleteItem is one (key, backend) pair fanned out to the worker
// pool during DeleteObjects.
type batchDeleteItem struct {
	key       string
	backend   s3be.ObjectBackend
	beName    string
	sizeBytes int64
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// DeleteObjects deletes multiple objects in a single request. Metadata
// removal happens in a single transaction via DeleteObjectsBatch; backend
// S3 deletes run concurrently with bounded parallelism to avoid
// overwhelming backends.
func (o *Manager) DeleteObjects(ctx context.Context, keys []string) []DeleteObjectResult {
	const operation = s3op.DeleteObjects
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation.String(),
		attribute.Int("s3o.batch_size", len(keys)),
	)
	defer span.End()

	results := make([]DeleteObjectResult, len(keys))
	for i, key := range keys {
		results[i].Key = key
	}

	copiesByKey, err := o.stores.DeleteObjectsBatch(ctx, keys)
	if err != nil {
		// Whole-tx failure: every key surfaces the error. The cache and
		// backend cleanup paths are skipped; nothing was changed.
		classified := o.core.ClassifyWriteError(span, operation.String(), err)
		for i := range results {
			results[i].Err = classified
		}
		return results
	}

	// A key absent from copiesByKey was already gone (not-found is silent
	// success), so its cache entries are also stale and worth flushing.
	for _, key := range keys {
		o.invalidateObjectCaches(key)
	}

	deleteItems := o.flattenBatchDeletes(ctx, copiesByKey)
	workerpool.Run(ctx, defaultBatchDeleteConcurrency, deleteItems, func(ctx context.Context, item batchDeleteItem) {
		o.coord.DeleteOrEnqueue(ctx, item.backend, item.beName, item.key, "batch_delete_failed", item.sizeBytes)
	})

	o.finalizeBatchDelete(ctx, span, len(keys), results, start)
	return results
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// flattenBatchDeletes produces the worker-pool input slice from the
// DeleteObjectsBatch result. Skips copies whose backend is unknown
// (logged). Per-backend DELETE API-call accounting happens inside
// DeleteOrEnqueue when the item is consumed, so no tick is recorded
// here.
func (o *Manager) flattenBatchDeletes(ctx context.Context, copiesByKey map[string][]core.DeletedCopy) []batchDeleteItem {
	var items []batchDeleteItem
	for key, copies := range copiesByKey {
		for _, cp := range copies {
			backend, ok := o.core.Backends()[cp.BackendName]
			if !ok {
				o.log.WarnContext(ctx, "backend not found for batch delete",
					"backend", cp.BackendName, "key", key)
				continue
			}
			items = append(items, batchDeleteItem{
				key: key, backend: backend, beName: cp.BackendName, sizeBytes: cp.SizeBytes,
			})
		}
	}
	return items
}

// tallyDeleteResults counts how many entries in results carry an error
// versus succeeded. Returned for metrics and audit logging.
func tallyDeleteResults(results []DeleteObjectResult) (int, int) {
	var successCount, errorCount int
	for _, r := range results {
		if r.Err != nil {
			errorCount++
		} else {
			successCount++
		}
	}
	return successCount, errorCount
}

// -------------------------------------------------------------------------------
// Object Manager - DELETE and Batch DELETE
//
// Author: Alex Freidah
//
// DeleteObject and DeleteObjects: the metadata delete runs in one transaction,
// then writepath.Coordinator.DeleteAllOrEnqueue removes the copies' bytes, one
// batched delete per backend. Success-finalization helpers live in
// mutation_finalize.go.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

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

	copies, _, err := o.stores.DeleteObject(ctx, key)
	if err != nil {
		if errors.Is(err, core.ErrObjectNotFound) {
			// Object not in our tracking - treat as success (idempotent delete)
			span.SetStatus(codes.Ok, "object not found - treating as success")
			return nil
		}
		return o.core.ClassifyWriteError(span, operation.String(), err)
	}
	// Credited by the same transaction that removed the rows, so a write racing
	// this delete sees the room the moment the ledger stops claiming the bytes.
	// The physical delete that follows can fail and leave an orphan, which the
	// cleanup queue owns.
	span.SetAttributes(attribute.Int("copies.deleted", len(copies)))

	// Drop the location cache entry first so concurrent readers are not sent
	// to a backend mid-delete during the fan-out.
	o.cache.Delete(key)

	reqs := make([]*core.CleanupRequest, len(copies))
	for i, cp := range copies {
		reqs[i] = cp.Cleanup(key, reasonDeleteFailed)
	}
	o.coord.DeleteAllOrEnqueue(ctx, reqs)

	o.finalizeDelete(ctx, span, key, copies, start)
	return nil
}

// Cleanup-queue reasons for a copy whose bytes a delete could not remove.
const (
	reasonDeleteFailed      = "delete_failed"
	reasonBatchDeleteFailed = "batch_delete_failed"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// DeleteObjectResult holds the outcome of a single key within a batch delete.
type DeleteObjectResult struct {
	Key string `json:"key,omitempty"`
	Err error  `json:"err,omitempty"`
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// DeleteObjects deletes multiple objects in a single request. Metadata removal
// happens in a single transaction via DeleteObjectsBatch; the copies' bytes are
// then removed one batched delete per backend.
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

	copiesByKey, _, err := o.stores.DeleteObjectsBatch(ctx, keys)
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

	var reqs []*core.CleanupRequest
	for key, copies := range copiesByKey {
		for _, cp := range copies {
			reqs = append(reqs, cp.Cleanup(key, reasonBatchDeleteFailed))
		}
	}
	o.coord.DeleteAllOrEnqueue(ctx, reqs)

	o.finalizeBatchDelete(ctx, span, len(keys), results, start)
	return results
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

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

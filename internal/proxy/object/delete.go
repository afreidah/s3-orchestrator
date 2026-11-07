// -------------------------------------------------------------------------------
// Object Manager - DELETE and Batch DELETE
//
// Author: Alex Freidah
//
// DeleteObject and DeleteObjects: the metadata delete runs in a transaction
// that also queues the copies' bytes for deletion, one per group of keys for a
// batch, then writepath.Coordinator.Settle deletes them, one batched delete per
// backend. Success-finalization helpers live in mutation_finalize.go.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"slices"
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

	copies, err := o.d.Stores.DeleteObject(ctx, key)
	if err != nil {
		if errors.Is(err, core.ErrObjectNotFound) {
			// Object not in our tracking - treat as success (idempotent delete)
			span.SetStatus(codes.Ok, "object not found - treating as success")
			return nil
		}
		return o.d.Core.ClassifyWriteError(span, operation, err)
	}
	// Credited by the same transaction that removed the rows, so a write racing
	// this delete sees the room the moment the ledger stops claiming the bytes.
	// That transaction also queued the bytes, so a delete that fails or never
	// runs leaves them to the cleanup worker.
	span.SetAttributes(attribute.Int("copies.deleted", len(copies)))

	// Drop the location cache entry first so concurrent readers are not sent
	// to a backend mid-delete during the fan-out.
	o.d.LocationCache.Delete(key)
	o.d.Coord.Settle(ctx, copies)

	o.finalizeDelete(ctx, span, key, copies, start)
	return nil
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// DeleteObjectResult holds the outcome of a single key within a batch delete.
type DeleteObjectResult struct {
	Key string `json:"key,omitempty"`
	Err error  `json:"err,omitempty"`
}

// deleteGroupSize is the most keys one delete transaction holds. Each key keeps
// a lock until the transaction ends, and every connection shares the database's
// lock table, so concurrent large batches could otherwise exhaust it.
const deleteGroupSize = 100

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// DeleteObjects deletes multiple objects in a single request. Metadata removal
// runs one transaction per group of keys, and a group that fails reports its
// own keys and leaves them in place. The bytes of every group that committed are
// then removed together, one batched delete per backend.
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

	var cleanup []core.CleanupItem
	failed := 0
	for group := range slices.Chunk(results, deleteGroupSize) {
		queued, err := o.deleteGroup(ctx, group)
		if err != nil {
			classified := o.d.Core.ClassifyWriteError(span, operation, err)
			for i := range group {
				group[i].Err = classified
			}
			failed += len(group)
			continue
		}
		cleanup = append(cleanup, queued...)
	}
	o.d.Coord.Settle(ctx, cleanup)

	if len(keys) > 0 && failed == len(keys) {
		return results
	}
	o.finalizeBatchDelete(ctx, span, len(keys), failed, start)
	return results
}

// deleteGroup removes one group's keys in a transaction and returns the cleanup
// rows it queued. A key that held no copies was already gone (not-found is
// silent success), so its location hint is also stale and worth dropping.
func (o *Manager) deleteGroup(ctx context.Context, group []DeleteObjectResult) ([]core.CleanupItem, error) {
	keys := make([]string, len(group))
	for i := range group {
		keys[i] = group[i].Key
	}
	cleanup, err := o.d.Stores.DeleteObjectsBatch(ctx, keys)
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		o.d.LocationCache.Delete(key)
	}
	return cleanup, nil
}

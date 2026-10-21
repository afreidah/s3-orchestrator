// -------------------------------------------------------------------------------
// Object Manager - Shared Mutation Finalization
//
// Author: Alex Freidah
//
// Per-operation success-finalization helpers shared by put.go, copy.go,
// and delete.go. Each helper owns the same checklist for its mutation
// kind: commit the metadata row (or skip when already committed),
// record per-backend accounting, emit operation-completion
// observability, and drop the key's degraded-mode location hint.
// Centralising the checklist here keeps the per-mutation rules in one
// place, so PUT and CopyObject usage counters cannot drift apart.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	pobserve "github.com/afreidah/s3-orchestrator/internal/proxy/observe"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// -------------------------------------------------------------------------
// PUT
// -------------------------------------------------------------------------

// finalizePutSuccess emits success metrics, audit log, and an event
// notification for a successful PutObject. Records failover spans when
// retries occurred.
func (o *Manager) finalizePutSuccess(ctx context.Context, span trace.Span, operation s3op.Operation, key, backendName string, size int64, start time.Time, failedBackends []string) {
	o.core.Acct().PutSuccess(ctx, operation, backendName, size, start)
	if len(failedBackends) > 0 {
		for _, fb := range failedBackends {
			telemetry.WriteFailoverTotal.WithLabelValues(operation.String(), fb, backendName).Inc()
		}
		span.SetAttributes(telemetry.AttrWriteFailover.Bool(true))
		span.SetAttributes(telemetry.AttrFailoverAttempts.Int(len(failedBackends)))
	}
	pobserve.PutCompleted(ctx, span, key, backendName, size)
	o.cache.Delete(key)
}

// -------------------------------------------------------------------------
// COPY
// -------------------------------------------------------------------------

// finalizeCopy records the destination of a successful copy from either path
// and runs its accounting. A server-side copy is charged as the one API call;
// a materialized copy adds the egress and ingress of the bytes that traversed
// the orchestrator. A record failure on a server-side copy still ends the copy,
// because its bytes are already on the destination.
func (o *Manager) finalizeCopy(ctx context.Context, req *copyContext, etag string, native bool) (string, error) {
	if err := o.coord.RecordObjectAndPromoteIntent(ctx, req.span, &core.RecordObjectRequest{
		Key: req.destKey, Size: req.size, Form: req.srcForm, Identity: req.identity, Tags: req.tags,
		Copies: []core.ObjectCopy{{Backend: req.destBackendName, IntentID: req.intentID, StorageKey: req.destStorageKey}},
	}); err != nil {
		return "", err
	}
	if req.identity.Complete() {
		etag = req.identity.ETag
	}
	o.core.Acct().Operation(s3op.CopyObject, req.destBackendName, req.start, nil)
	if native {
		req.span.SetAttributes(telemetry.AttrNativeCopy.Bool(true))
	} else {
		o.core.Acct().Egress(ctx, s3op.GetObject, req.srcBackendName, req.size)
		o.core.Acct().Ingress(ctx, s3op.PutObject, req.destBackendName, req.size)
	}
	pobserve.CopyCompleted(ctx, req.span, req.sourceKey, req.destKey, req.srcBackendName, req.destBackendName, req.size)
	o.cache.Delete(req.destKey)
	return etag, nil
}

// -------------------------------------------------------------------------
// DELETE
// -------------------------------------------------------------------------

// finalizeDelete runs the post-fanout success steps for single-key
// DeleteObject: operation-completion accounting (pinned to the first
// copy's backend for label stability) and completion observability.
// Per-backend DELETE API-call accounting is owned by the cleanup settler
// so this helper does NOT call APICall.
func (o *Manager) finalizeDelete(ctx context.Context, span trace.Span, key string, copies []core.CleanupItem, start time.Time) {
	const operation = s3op.DeleteObject
	if len(copies) > 0 {
		o.core.Acct().Operation(operation, copies[0].BackendName, start, nil)
	}
	pobserve.DeleteCompleted(ctx, span, key, len(copies))
}

// finalizeBatchDelete runs the post-fanout success steps for
// DeleteObjects: operation-completion accounting (empty backend label
// because the batch spans many), per-key tally span attributes, and
// completion observability. Per-key API-call accounting is owned by
// the cleanup settler.
func (o *Manager) finalizeBatchDelete(ctx context.Context, span trace.Span, batchSize int, results []DeleteObjectResult, start time.Time) {
	const operation = s3op.DeleteObjects
	successCount, errorCount := tallyDeleteResults(results)
	o.core.Acct().Operation(operation, "", start, nil)
	span.SetAttributes(
		attribute.Int("s3o.deleted_count", successCount),
		attribute.Int("s3o.error_count", errorCount),
	)
	pobserve.DeleteBatchCompleted(ctx, span, batchSize, successCount, errorCount)
}

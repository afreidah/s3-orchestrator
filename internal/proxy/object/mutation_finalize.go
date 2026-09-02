// -------------------------------------------------------------------------------
// Object Manager - Shared Mutation Finalization
//
// Author: Alex Freidah
//
// Per-operation success-finalization helpers shared by put.go, copy.go,
// and delete.go. Each helper owns the same checklist for its mutation
// kind: commit the metadata row (or skip when already committed),
// record per-backend accounting, emit operation-completion
// observability, and invalidate every cache tied to the key.
// Centralising the checklist here keeps the per-mutation rules in one
// place — a missing accounting call here previously surfaced as drift
// between PUT and CopyObject usage counters.
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

// finalizePutSuccess emits success metrics, audit log, and an event
// notification for a successful PutObject. Records failover spans when
// retries occurred.
func (o *Manager) finalizePutSuccess(ctx context.Context, span trace.Span, operation s3op.Operation, key, backendName string, size int64, start time.Time, failedBackends []string) {
	o.core.Acct().PutSuccess(operation, backendName, size, start)
	if len(failedBackends) > 0 {
		for _, fb := range failedBackends {
			telemetry.WriteFailoverTotal.WithLabelValues(operation.String(), fb, backendName).Inc()
		}
		span.SetAttributes(telemetry.AttrWriteFailover.Bool(true))
		span.SetAttributes(telemetry.AttrFailoverAttempts.Int(len(failedBackends)))
	}
	pobserve.PutCompleted(ctx, span, key, backendName, size)
	o.invalidateObjectCaches(key)
}

// finalizeMaterializedCopy runs the post-PUT-success steps for the
// stream-through copy path. Differs from finalizeNativeCopy by adding
// the egress/ingress tick because the bytes physically traversed the
// orchestrator.
func (o *Manager) finalizeMaterializedCopy(ctx context.Context, req *materializedCopyContext, etag string) (string, error) {
	const operation = s3op.CopyObject
	if err := o.coord.RecordObjectOrCleanup(ctx, req.span, req.destBackend, &core.RecordObjectRequest{
		Key: req.destKey, Backend: req.destBackendName, Size: req.size, Form: req.srcForm,
		Identity: req.identity, Tags: req.tags,
	}); err != nil {
		return "", err
	}
	if req.identity.Complete() {
		etag = req.identity.ETag
	}
	o.core.Acct().Operation(operation, req.destBackendName, req.start, nil)
	o.core.Acct().Egress(s3op.GetObject, req.srcBackendName, req.size)
	o.core.Acct().Ingress(s3op.PutObject, req.destBackendName, req.size)
	pobserve.CopyCompleted(ctx, req.span, req.sourceKey, req.destKey, req.srcBackendName, req.destBackendName, req.size)
	o.invalidateObjectCaches(req.destKey)
	return etag, nil
}

// finalizeNativeCopy runs the post-native-copy success steps shared by
// the happy path and the HEAD-probe recovery path: record the
// destination location, refresh accounting, mark the span as a native
// copy, emit completion observability, and invalidate caches. Returns
// (_, true, err) on RecordObjectOrCleanup failure - the bytes are
// already on the destination so the caller MUST NOT fall back.
func (o *Manager) finalizeNativeCopy(ctx context.Context, req *nativeCopyContext, etag string) (string, bool, error) {
	const operation = s3op.CopyObject
	if err := o.coord.RecordObjectOrCleanup(ctx, req.span, req.destBackend, &core.RecordObjectRequest{
		Key: req.destKey, Backend: req.destBackendName, Size: req.size, Form: req.srcForm,
		Identity: req.identity, Tags: req.tags,
	}); err != nil {
		return "", true, err
	}
	if req.identity.Complete() {
		etag = req.identity.ETag
	}
	o.core.Acct().Operation(operation, req.destBackendName, req.start, nil)
	req.span.SetAttributes(telemetry.AttrNativeCopy.Bool(true))
	pobserve.CopyCompleted(ctx, req.span, req.sourceKey, req.destKey, req.destBackendName, req.destBackendName, req.size)
	o.invalidateObjectCaches(req.destKey)
	return etag, true, nil
}

// finalizeDelete runs the post-fanout success steps for single-key
// DeleteObject: operation-completion accounting (pinned to the first
// copy's backend for label stability), completion observability, and
// cache invalidation. Per-backend DELETE API-call accounting is owned
// by DeleteOrEnqueue so this helper does NOT call APICall.
func (o *Manager) finalizeDelete(ctx context.Context, span trace.Span, key string, copies []core.DeletedCopy, start time.Time) {
	const operation = s3op.DeleteObject
	if len(copies) > 0 {
		o.core.Acct().Operation(operation, copies[0].BackendName, start, nil)
	}
	pobserve.DeleteCompleted(ctx, span, key, len(copies))
	o.invalidateObjectCaches(key)
}

// finalizeBatchDelete runs the post-fanout success steps for
// DeleteObjects: operation-completion accounting (empty backend label
// because the batch spans many), per-key tally span attributes, and
// completion observability. Per-key API-call accounting is owned by
// DeleteOrEnqueue inside the fanout.
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

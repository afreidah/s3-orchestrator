// -------------------------------------------------------------------------------
// Per-Operation Completion Observability
//
// Author: Alex Freidah
//
// One helper per storage operation that bundles audit.Log + event.Publish +
// span.SetStatus so the audit signature for "storage.PutObject" lives in
// exactly one place and cannot drift across the object/, multipart/, and
// writepath/ call sites. Accounting and cache invalidation are NOT here:
// accounting is centralized in internal/proxy/accounting; cache
// invalidation stays at the call site (intentionally inconsistent across
// read/write/list).
// -------------------------------------------------------------------------------

// Package observe centralizes per-operation completion observability  -
// audit log, notification event, and span status  -  so storage paths in
// object/, multipart/, and writepath/ can mark success with one call and
// the audit/event attribute shapes stay defined in exactly one place.
package observe

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
)

// -------------------------------------------------------------------------
// WRITE-PATH COMPLETIONS
// -------------------------------------------------------------------------

// PutCompleted marks a successful PutObject. Emits the
// storage.PutObject audit log, an s3:ObjectCreated:Put notification, and
// sets the span to Ok.
func PutCompleted(ctx context.Context, span trace.Span, key, backend string, size int64) {
	audit.Log(ctx, "storage.PutObject",
		slog.String("key", key),
		slog.String("backend", backend),
		slog.Int64("size", size),
	)
	bucket, userKey := internalkey.Split(key)
	event.Publish(event.ObjectCreatedPut, userKey, map[string]any{
		"bucket":     bucket,
		"key":        userKey,
		"backend":    backend,
		"size":       size,
		"request_id": audit.RequestID(ctx),
	})
	span.SetStatus(codes.Ok, "")
}

// CopyCompleted marks a successful CopyObject. Emits the
// storage.CopyObject audit log, an s3:ObjectCreated:Copy notification, and
// sets the span to Ok.
func CopyCompleted(ctx context.Context, span trace.Span, sourceKey, destKey, sourceBackend, destBackend string, size int64) {
	audit.Log(ctx, "storage.CopyObject",
		slog.String("source_key", sourceKey),
		slog.String("dest_key", destKey),
		slog.String("src_backend", sourceBackend),
		slog.String("dest_backend", destBackend),
		slog.Int64("size", size),
	)
	bucket, userKey := internalkey.Split(destKey)
	event.Publish(event.ObjectCreatedCopy, userKey, map[string]any{
		"bucket":     bucket,
		"key":        userKey,
		"source_key": sourceKey,
		"backend":    destBackend,
		"size":       size,
		"request_id": audit.RequestID(ctx),
	})
	span.SetStatus(codes.Ok, "")
}

// DeleteCompleted marks a successful DeleteObject. Emits the
// storage.DeleteObject audit log, an s3:ObjectRemoved:Delete notification,
// and sets the span to Ok.
func DeleteCompleted(ctx context.Context, span trace.Span, key string, copiesDeleted int) {
	audit.Log(ctx, "storage.DeleteObject",
		slog.String("key", key),
		slog.Int("copies_deleted", copiesDeleted),
	)
	bucket, userKey := internalkey.Split(key)
	event.Publish(event.ObjectRemovedDelete, userKey, map[string]any{
		"bucket":         bucket,
		"key":            userKey,
		"copies_deleted": copiesDeleted,
		"request_id":     audit.RequestID(ctx),
	})
	span.SetStatus(codes.Ok, "")
}

// DeleteBatchCompleted marks a successful DeleteObjects batch. Emits the
// storage.DeleteObjects audit log, an s3:ObjectRemoved:DeleteBatch
// notification, and sets the span to Ok. The caller is responsible for
// any per-key span attributes; this helper does not synthesize a Subject
// (batch operations span many keys).
func DeleteBatchCompleted(ctx context.Context, span trace.Span, totalKeys, deleted, errors int) {
	audit.Log(ctx, "storage.DeleteObjects",
		slog.Int("total_keys", totalKeys),
		slog.Int("deleted", deleted),
		slog.Int("errors", errors),
	)
	event.Publish(event.ObjectRemovedDeleteBatch, "", map[string]any{
		"total_keys": totalKeys,
		"deleted":    deleted,
		"errors":     errors,
		"request_id": audit.RequestID(ctx),
	})
	span.SetStatus(codes.Ok, "")
}

// -------------------------------------------------------------------------
// READ-PATH COMPLETIONS
// -------------------------------------------------------------------------

// GetCompleted marks a successful GetObject. Emits the storage.GetObject
// audit log. Used for both backend reads (backend = backend name) and
// data-cache hits (backend = "cache"). Does not set span status: the read
// path currently relies on span.End for completion.
func GetCompleted(ctx context.Context, key, backend string, size int64) {
	audit.Log(ctx, "storage.GetObject",
		slog.String("key", key),
		slog.String("backend", backend),
		slog.Int64("size", size),
	)
}

// HeadCompleted marks a successful HeadObject. Emits the
// storage.HeadObject audit log. Does not set span status (parity with
// today's behavior).
func HeadCompleted(ctx context.Context, key, backend string, size int64) {
	audit.Log(ctx, "storage.HeadObject",
		slog.String("key", key),
		slog.String("backend", backend),
		slog.Int64("size", size),
	)
}

// ListCompleted marks a successful ListObjects. Emits the
// storage.ListObjects audit log. Does not set span status (parity with
// today's behavior).
func ListCompleted(ctx context.Context, prefix string, keyCount int, truncated bool) {
	audit.Log(ctx, "storage.ListObjects",
		slog.String("prefix", prefix),
		slog.Int("key_count", keyCount),
		slog.Bool("truncated", truncated),
	)
}

// -------------------------------------------------------------------------
// MULTIPART COMPLETIONS
// -------------------------------------------------------------------------

// MultipartCreated marks a successful CreateMultipartUpload. Emits the
// storage.CreateMultipartUpload audit log and sets the span to Ok. No
// notification event is emitted: AWS S3 itself does not emit a
// notification for CreateMultipartUpload, only for the final
// CompleteMultipartUpload.
func MultipartCreated(ctx context.Context, span trace.Span, key, backend, uploadID string) {
	audit.Log(ctx, "storage.CreateMultipartUpload",
		slog.String("key", key),
		slog.String("backend", backend),
		slog.String("upload_id", uploadID),
	)
	span.SetStatus(codes.Ok, "")
}

// UploadPartCompleted marks a successful UploadPart. Emits the
// storage.UploadPart audit log and sets the span to Ok. No notification
// event is emitted: AWS S3 itself does not emit per-part notifications,
// only for the final CompleteMultipartUpload, so adding one here would
// expand notification cardinality beyond the S3 contract.
func UploadPartCompleted(ctx context.Context, span trace.Span, key, backend, uploadID string, partNumber int, size int64) {
	audit.Log(ctx, "storage.UploadPart",
		slog.String("key", key),
		slog.String("backend", backend),
		slog.String("upload_id", uploadID),
		slog.Int("part_number", partNumber),
		slog.Int64("size", size),
	)
	span.SetStatus(codes.Ok, "")
}

// MultipartAborted marks a successful AbortMultipartUpload. Emits the
// storage.AbortMultipartUpload audit log and sets the span to Ok. No
// notification event is emitted: AWS S3 itself does not emit a
// notification for an aborted upload.
func MultipartAborted(ctx context.Context, span trace.Span, uploadID, key, backend string, partsCleaned int) {
	audit.Log(ctx, "storage.AbortMultipartUpload",
		slog.String("upload_id", uploadID),
		slog.String("key", key),
		slog.String("backend", backend),
		slog.Int("parts_cleaned", partsCleaned),
	)
	span.SetStatus(codes.Ok, "")
}

// MultipartCompleted marks a successful CompleteMultipartUpload. Emits
// the storage.CompleteMultipartUpload audit log, an
// s3:ObjectCreated:CompleteMultipartUpload notification, and sets the span
// to Ok.
func MultipartCompleted(ctx context.Context, span trace.Span, key, backend, uploadID string, totalSize int64, partsCount int) {
	audit.Log(ctx, "storage.CompleteMultipartUpload",
		slog.String("key", key),
		slog.String("backend", backend),
		slog.String("upload_id", uploadID),
		slog.Int64("total_size", totalSize),
		slog.Int("parts_count", partsCount),
	)
	bucket, userKey := internalkey.Split(key)
	event.Publish(event.ObjectCreatedCompleteMultipartUpload, userKey, map[string]any{
		"bucket":      bucket,
		"key":         userKey,
		"backend":     backend,
		"size":        totalSize,
		"parts_count": partsCount,
		"upload_id":   uploadID,
		"request_id":  audit.RequestID(ctx),
	})
	span.SetStatus(codes.Ok, "")
}

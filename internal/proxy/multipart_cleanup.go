// -------------------------------------------------------------------------------
// Multipart Manager - Abort and Stale Cleanup
//
// Author: Alex Freidah
//
// Client-driven AbortMultipartUpload (scoped by bucket/key to prevent
// cross-bucket probing) plus the internal sweepers that the multipart
// cleanup background worker and backend-drain flow invoke. All three
// paths converge on abortByMultipartRow, which deletes every part object
// through the write coordinator's enqueueing cleanup primitive, removes
// the upload row, evicts any cached DEK, and records the abort against
// the backend's usage counter.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/codes"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// AbortMultipartUpload cleans up an in-progress multipart upload, removing
// all part objects from the backend and the upload records from the database.
// The bucket/key arguments scope the operation to the requesting client's
// URL, matching them against the stored ObjectKey via validateMultipartScope
// so a caller for one bucket cannot abort an upload that belongs to another.
func (mp *MultipartManager) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	const operation = "AbortMultipartUpload"
	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
		telemetry.AttrUploadID.String(uploadID),
	)
	defer span.End()
	mu, err := mp.fetchScopedUpload(ctx, span, bucket, key, uploadID, operation)
	if err != nil {
		return err
	}
	return mp.abortByMultipartRow(ctx, mu)
}

// abortByMultipartRow performs the actual abort given a resolved
// MultipartUpload row. Internal callers (CleanupStaleMultipartUploads,
// AbortMultipartUploadsOnBackend) bypass the bucket-scope check because
// they operate on the entire upload set, not a per-request URL.
func (mp *MultipartManager) abortByMultipartRow(ctx context.Context, mu *core.MultipartUpload) error {
	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+"AbortMultipartUpload",
		telemetry.AttrUploadID.String(mu.UploadID),
	)
	defer span.End()
	const operation = "AbortMultipartUpload"
	start := time.Now()
	uploadID := mu.UploadID

	be, err := mp.GetBackend(mu.BackendName)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	parts, err := mp.stores.GetParts(ctx, uploadID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return fmt.Errorf("failed to get parts for abort: %w", err)
	}

	for _, part := range parts {
		partKey := multipartPartKey(uploadID, part.PartNumber)
		mp.coord.DeleteOrEnqueue(ctx, be, mu.BackendName, partKey, "abort_part_cleanup", part.SizeBytes)
	}

	if err := mp.stores.DeleteMultipartUpload(ctx, uploadID); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	mp.forgetUploadDEK(uploadID)

	mp.recordOperation(operation, mu.BackendName, start, nil)
	// 1 abort. The N part DELETEs go through DeleteOrEnqueue, which
	// records them itself.
	mp.usage.Record(mu.BackendName, 1, 0, 0)

	audit.Log(ctx, "storage.AbortMultipartUpload",
		slog.String("upload_id", uploadID),
		slog.String("key", mu.ObjectKey),
		slog.String("backend", mu.BackendName),
		slog.Int("parts_cleaned", len(parts)),
	)

	span.SetStatus(codes.Ok, "")
	return nil
}

// CleanupStaleMultipartUploads aborts multipart uploads older than the given
// duration. Run periodically to prevent quota leaks from abandoned uploads.
func (mp *MultipartManager) CleanupStaleMultipartUploads(ctx context.Context, olderThan time.Duration) {
	uploads, err := mp.stores.GetStaleMultipartUploads(ctx, olderThan)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get stale multipart uploads", "error", err)
		return
	}

	cleaned := 0
	for i := range uploads {
		mu := &uploads[i]
		slog.InfoContext(ctx, "cleaning up stale multipart upload", "upload_id", mu.UploadID, "key", mu.ObjectKey)
		if err := mp.abortByMultipartRow(ctx, mu); err != nil {
			slog.ErrorContext(ctx, "failed to clean up upload", "upload_id", mu.UploadID, "error", err)
		} else {
			cleaned++
		}
	}

	if cleaned > 0 {
		audit.Log(ctx, "storage.MultipartCleanup",
			slog.Int("cleaned", cleaned),
			slog.Int("total_stale", len(uploads)),
		)
	}
}

// AbortMultipartUploadsOnBackend aborts all in-progress multipart uploads
// on the given backend.
func (mp *MultipartManager) AbortMultipartUploadsOnBackend(ctx context.Context, backendName string) {
	uploads, err := mp.stores.GetMultipartUploadsByBackend(ctx, backendName)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list multipart uploads", "backend", backendName, "error", err)
		return
	}

	for i := range uploads {
		mu := &uploads[i]
		slog.InfoContext(ctx, "aborting multipart upload", "upload_id", mu.UploadID, "key", mu.ObjectKey)
		if err := mp.abortByMultipartRow(ctx, mu); err != nil {
			slog.ErrorContext(ctx, "failed to abort multipart upload",
				"upload_id", mu.UploadID, "error", err)
		}
	}
}

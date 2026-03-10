// -------------------------------------------------------------------------------
// Multipart Manager - Multipart Upload Lifecycle
//
// Author: Alex Freidah
//
// Handles multipart upload operations: create, upload part, complete, abort,
// list, and stale upload cleanup. Backend selection for new uploads uses the
// configured routing strategy via backendCore.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// MultipartManager handles the multipart upload lifecycle.
type MultipartManager struct {
	*backendCore
	encryptor *encryption.Encryptor
}

// NewMultipartManager creates a MultipartManager sharing the given core
// infrastructure and optional encryptor.
func NewMultipartManager(core *backendCore, encryptor *encryption.Encryptor) *MultipartManager {
	return &MultipartManager{backendCore: core, encryptor: encryptor}
}

// multipartPartKey returns the temporary object key for a multipart part.
func multipartPartKey(uploadID string, partNumber int) string {
	return fmt.Sprintf("__multipart/%s/%d", uploadID, partNumber)
}

// -------------------------------------------------------------------------
// MULTIPART UPLOAD OPERATIONS
// -------------------------------------------------------------------------

// CreateMultipartUpload initiates a multipart upload by selecting a backend
// with available quota and recording the upload in the database.
func (mp *MultipartManager) CreateMultipartUpload(ctx context.Context, key, contentType string, metadata map[string]string) (string, string, error) {
	const operation = "CreateMultipartUpload"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()

	// Filter backends within usage limits, exclude draining and unhealthy
	eligible := mp.excludeUnhealthy(mp.excludeDraining(mp.usage.BackendsWithinLimits(mp.order, 1, 0, 0)))
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "write").Inc()
		span.SetStatus(codes.Error, "usage limits exceeded on all backends")
		return "", "", ErrInsufficientStorage
	}

	// Pick a backend (estimate 0 bytes since final size is unknown)
	backendName, err := mp.selectBackendForWrite(ctx, 0, eligible)
	if err != nil {
		return "", "", mp.classifyWriteError(span, operation, err)
	}

	uploadID := GenerateUploadID()
	if err := mp.store.CreateMultipartUpload(ctx, uploadID, key, backendName, contentType, metadata); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return "", "", err
	}

	span.SetAttributes(telemetry.AttrBackendName.String(backendName))
	mp.recordOperation(operation, backendName, start, nil)

	audit.Log(ctx, "storage.CreateMultipartUpload",
		slog.String("key", key),
		slog.String("backend", backendName),
		slog.String("upload_id", uploadID),
	)

	span.SetStatus(codes.Ok, "")
	return uploadID, backendName, nil
}

// UploadPart uploads a single part to the backend. Parts are stored under a
// temporary key prefix and reassembled on completion.
func (mp *MultipartManager) UploadPart(ctx context.Context, uploadID string, partNumber int, body io.Reader, size int64) (string, error) {
	const operation = "UploadPart"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		attribute.String("s3.upload_id", uploadID),
		attribute.Int("s3.part_number", partNumber),
	)
	defer span.End()

	if partNumber < 1 || partNumber > 10000 {
		err := &S3Error{StatusCode: 400, Code: "InvalidArgument", Message: "Part number must be between 1 and 10000"}
		span.SetStatus(codes.Error, err.Message)
		return "", err
	}

	mu, err := mp.store.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return "", mp.classifyWriteError(span, operation, err)
	}

	backend, err := mp.getBackend(mu.BackendName)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	// Check usage limits before uploading
	if !mp.usage.WithinLimits(mu.BackendName, 1, 0, size) {
		span.SetStatus(codes.Error, "usage limits exceeded")
		return "", ErrInsufficientStorage
	}

	// Encrypt if enabled
	var enc *EncryptionMeta
	uploadBody := body
	uploadSize := size
	if mp.encryptor != nil {
		encResult, encErr := mp.encryptor.Encrypt(ctx, body, size)
		if encErr != nil {
			telemetry.EncryptionErrorsTotal.WithLabelValues("encrypt", "encrypt_failed").Inc()
			span.SetStatus(codes.Error, encErr.Error())
			return "", fmt.Errorf("encrypt part: %w", encErr)
		}
		telemetry.EncryptionOpsTotal.WithLabelValues("encrypt").Inc()
		uploadBody = encResult.Body
		uploadSize = encResult.CiphertextSize
		enc = &EncryptionMeta{
			Encrypted:     true,
			EncryptionKey: encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK),
			KeyID:         encResult.KeyID,
			PlaintextSize: size,
		}
	}

	// Store part under a temp key
	partKey := multipartPartKey(uploadID, partNumber)
	bctx, bcancel := mp.withTimeout(ctx)
	defer bcancel()
	etag, err := backend.PutObject(bctx, partKey, uploadBody, uploadSize, "application/octet-stream", nil)
	if err != nil {
		mp.usage.Record(mu.BackendName, 1, 0, 0) // API call was made even on failure
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("failed to upload part: %w", err)
	}

	if err := mp.store.RecordPart(ctx, uploadID, partNumber, etag, uploadSize, enc); err != nil {
		slog.Error("RecordPart failed, cleaning up part object",
			"upload_id", uploadID, "part", partNumber, "error", err)
		mp.usage.Record(mu.BackendName, 1, 0, 0)
		if delErr := backend.DeleteObject(ctx, partKey); delErr != nil {
			slog.Error("Failed to clean up orphaned part object",
				"key", partKey, "error", delErr)
			mp.enqueueCleanup(ctx, mu.BackendName, partKey, "orphan_part_record_failed", uploadSize)
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return "", fmt.Errorf("failed to record part: %w", err)
	}

	mp.recordOperation(operation, mu.BackendName, start, nil)
	mp.usage.Record(mu.BackendName, 1, 0, size)
	span.SetStatus(codes.Ok, "")
	return etag, nil
}

// CompleteMultipartUpload reassembles parts into the final object. Downloads
// each part, concatenates them into a single upload, then cleans up temp keys
// and records the final object location with quota tracking.
func (mp *MultipartManager) CompleteMultipartUpload(ctx context.Context, uploadID string, partNumbers []int) (string, error) {
	const operation = "CompleteMultipartUpload"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		attribute.String("s3.upload_id", uploadID),
	)
	defer span.End()

	mu, err := mp.store.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return "", mp.classifyWriteError(span, operation, err)
	}

	backend, err := mp.getBackend(mu.BackendName)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	allParts, err := mp.store.GetParts(ctx, uploadID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	// Filter to only the parts specified by the client
	requested := make(map[int]bool, len(partNumbers))
	for _, pn := range partNumbers {
		requested[pn] = true
	}
	var parts []MultipartPart
	for _, p := range allParts {
		if requested[p.PartNumber] {
			parts = append(parts, p)
		}
	}
	if len(parts) != len(partNumbers) {
		err := &S3Error{StatusCode: 400, Code: "InvalidPart", Message: "one or more specified parts were not uploaded"}
		span.SetStatus(codes.Error, err.Message)
		return "", err
	}

	// Sort parts by part number for correct assembly order
	slices.SortFunc(parts, func(a, b MultipartPart) int {
		return a.PartNumber - b.PartNumber
	})

	// Calculate total plaintext size for the combined upload. When parts are
	// encrypted, PlaintextSize holds the original size; otherwise SizeBytes.
	var totalPlaintextSize int64
	anyEncrypted := false
	for _, part := range parts {
		if part.Encrypted {
			totalPlaintextSize += part.PlaintextSize
			anyEncrypted = true
		} else {
			totalPlaintextSize += part.SizeBytes
		}
	}

	// Stream parts sequentially through a pipe. When parts are encrypted,
	// each part is decrypted inline so the pipe carries plaintext. The main
	// goroutine then optionally re-encrypts as a single final object.
	pr, pw := io.Pipe()
	pipeCtx, pipeCancel := context.WithCancel(ctx)
	defer pipeCancel()

	go func() {
		defer func() { _ = pw.Close() }()
		for _, part := range parts {
			partKey := multipartPartKey(uploadID, part.PartNumber)
			bctx, bcancel := mp.withTimeout(pipeCtx)
			result, err := backend.GetObject(bctx, partKey, "")
			if err != nil {
				bcancel()
				pw.CloseWithError(fmt.Errorf("failed to read part %d: %w", part.PartNumber, err))
				return
			}

			var src io.Reader = result.Body
			if part.Encrypted && mp.encryptor != nil {
				_, wrappedDEK, unpackErr := encryption.UnpackKeyData(part.EncryptionKey)
				if unpackErr != nil {
					telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt", "unpack_failed").Inc()
					_ = result.Body.Close()
					bcancel()
					pw.CloseWithError(fmt.Errorf("unpack part %d key: %w", part.PartNumber, unpackErr))
					return
				}
				decrypted, decErr := mp.encryptor.Decrypt(pipeCtx, result.Body, wrappedDEK, part.KeyID)
				if decErr != nil {
					telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt", "decrypt_failed").Inc()
					_ = result.Body.Close()
					bcancel()
					pw.CloseWithError(fmt.Errorf("decrypt part %d: %w", part.PartNumber, decErr))
					return
				}
				telemetry.EncryptionOpsTotal.WithLabelValues("decrypt").Inc()
				src = decrypted
			}

			_, err = io.Copy(pw, src)
			_ = result.Body.Close()
			bcancel()
			if err != nil {
				pw.CloseWithError(fmt.Errorf("failed to stream part %d: %w", part.PartNumber, err))
				return
			}
		}
	}()

	// When encryption is enabled, re-encrypt the combined plaintext as a
	// single object with unified chunk boundaries. Otherwise upload as-is.
	var enc *EncryptionMeta
	var uploadBody io.Reader = pr
	uploadSize := totalPlaintextSize
	if mp.encryptor != nil {
		encResult, encErr := mp.encryptor.Encrypt(ctx, pr, totalPlaintextSize)
		if encErr != nil {
			telemetry.EncryptionErrorsTotal.WithLabelValues("encrypt", "encrypt_failed").Inc()
			span.SetStatus(codes.Error, encErr.Error())
			return "", fmt.Errorf("encrypt final object: %w", encErr)
		}
		telemetry.EncryptionOpsTotal.WithLabelValues("encrypt").Inc()
		uploadBody = encResult.Body
		uploadSize = encResult.CiphertextSize
		enc = &EncryptionMeta{
			Encrypted:     true,
			EncryptionKey: encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK),
			KeyID:         encResult.KeyID,
			PlaintextSize: totalPlaintextSize,
		}
	} else if anyEncrypted {
		// Parts were encrypted but encryptor is now nil (disabled). The pipe
		// carries decrypted plaintext; upload without encryption metadata.
		uploadSize = totalPlaintextSize
	}

	// Streamed from pipe; deadline governed by the caller's context.
	etag, err := backend.PutObject(ctx, mu.ObjectKey, uploadBody, uploadSize, mu.ContentType, mu.Metadata)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("failed to upload final object: %w", err)
	}

	// Record the final object location and update quota
	if err := mp.recordObjectOrCleanup(ctx, span, backend, mu.ObjectKey, mu.BackendName, uploadSize, enc); err != nil {
		return "", err
	}

	// Clean up part objects from backend
	for _, part := range parts {
		partKey := multipartPartKey(uploadID, part.PartNumber)
		mp.deleteOrEnqueue(ctx, backend, mu.BackendName, partKey, "complete_part_cleanup", part.SizeBytes)
	}

	// Clean up multipart records from database
	if err := mp.store.DeleteMultipartUpload(ctx, uploadID); err != nil {
		span.RecordError(err)
	}

	mp.recordOperation(operation, mu.BackendName, start, nil)
	// API calls: N GetObject (read parts) + 1 PutObject (assembled) + N DeleteObject (cleanup)
	mp.usage.Record(mu.BackendName, int64(2*len(parts)+1), 0, uploadSize)

	audit.Log(ctx, "storage.CompleteMultipartUpload",
		slog.String("key", mu.ObjectKey),
		slog.String("backend", mu.BackendName),
		slog.String("upload_id", uploadID),
		slog.Int64("total_size", totalPlaintextSize),
		slog.Int("parts_count", len(parts)),
	)

	span.SetStatus(codes.Ok, "")
	return etag, nil
}

// AbortMultipartUpload cleans up an in-progress multipart upload, removing
// all part objects from the backend and the upload records from the database.
func (mp *MultipartManager) AbortMultipartUpload(ctx context.Context, uploadID string) error {
	const operation = "AbortMultipartUpload"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		attribute.String("s3.upload_id", uploadID),
	)
	defer span.End()

	mu, err := mp.store.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return mp.classifyWriteError(span, operation, err)
	}

	backend, err := mp.getBackend(mu.BackendName)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	parts, err := mp.store.GetParts(ctx, uploadID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return fmt.Errorf("failed to get parts for abort: %w", err)
	}

	// Delete part objects from backend
	for _, part := range parts {
		partKey := multipartPartKey(uploadID, part.PartNumber)
		mp.deleteOrEnqueue(ctx, backend, mu.BackendName, partKey, "abort_part_cleanup", part.SizeBytes)
	}

	// Delete multipart records from database
	if err := mp.store.DeleteMultipartUpload(ctx, uploadID); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	mp.recordOperation(operation, mu.BackendName, start, nil)
	mp.usage.Record(mu.BackendName, int64(len(parts)+1), 0, 0) // N deletes + 1 abort

	audit.Log(ctx, "storage.AbortMultipartUpload",
		slog.String("upload_id", uploadID),
		slog.String("key", mu.ObjectKey),
		slog.String("backend", mu.BackendName),
		slog.Int("parts_cleaned", len(parts)),
	)

	span.SetStatus(codes.Ok, "")
	return nil
}

// ListMultipartUploads returns active multipart uploads matching the given
// prefix, up to maxUploads results. Pass-through to the metadata store.
func (mp *MultipartManager) ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]MultipartUpload, error) {
	return mp.store.ListMultipartUploads(ctx, prefix, maxUploads)
}

// GetParts returns all parts for a multipart upload.
func (mp *MultipartManager) GetParts(ctx context.Context, uploadID string) ([]MultipartPart, error) {
	return mp.store.GetParts(ctx, uploadID)
}

// CleanupStaleMultipartUploads aborts multipart uploads older than the given
// duration. Run periodically to prevent quota leaks from abandoned uploads.
func (mp *MultipartManager) CleanupStaleMultipartUploads(ctx context.Context, olderThan time.Duration) {
	uploads, err := mp.store.GetStaleMultipartUploads(ctx, olderThan)
	if err != nil {
		slog.Error("Failed to get stale multipart uploads", "error", err)
		return
	}

	cleaned := 0
	for _, mu := range uploads {
		slog.Info("Cleaning up stale multipart upload", "upload_id", mu.UploadID, "key", mu.ObjectKey)
		if err := mp.AbortMultipartUpload(ctx, mu.UploadID); err != nil {
			slog.Error("Failed to clean up upload", "upload_id", mu.UploadID, "error", err)
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

// abortMultipartUploadsOnBackend aborts all in-progress multipart uploads
// on the given backend.
func (mp *MultipartManager) abortMultipartUploadsOnBackend(ctx context.Context, backendName string) {
	uploads, err := mp.store.GetStaleMultipartUploads(ctx, 0)
	if err != nil {
		slog.Error("Drain: failed to list multipart uploads", "error", err)
		return
	}

	for _, mu := range uploads {
		if mu.BackendName != backendName {
			continue
		}
		slog.Info("Drain: aborting multipart upload", "upload_id", mu.UploadID, "key", mu.ObjectKey)
		if err := mp.AbortMultipartUpload(ctx, mu.UploadID); err != nil {
			slog.Error("Drain: failed to abort multipart upload",
				"upload_id", mu.UploadID, "error", err)
		}
	}
}

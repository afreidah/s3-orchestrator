// -------------------------------------------------------------------------------
// Multipart Manager - Multipart Upload Lifecycle
//
// Author: Alex Freidah
//
// Handles multipart upload operations: create, upload part, complete, abort,
// list, and stale upload cleanup. Backend selection for new uploads uses the
// configured routing strategy via backendCore.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// MultipartManager handles the multipart upload lifecycle.
type MultipartManager struct {
	*backendCore
	parent      *BackendManager // set post-construction; routes write-path helpers to the parent's store fields
	encryptor   *encryption.Encryptor
	objectCache objcache.ObjectCache
}

// NewMultipartManager creates a MultipartManager sharing the given core
// infrastructure and optional encryptor. The caller wires the parent
// BackendManager pointer after construction.
func NewMultipartManager(core *backendCore, encryptor *encryption.Encryptor, objectCache objcache.ObjectCache) *MultipartManager {
	return &MultipartManager{backendCore: core, encryptor: encryptor, objectCache: objectCache}
}

// multipartPartKey returns the temporary object key for a multipart part.
func multipartPartKey(uploadID string, partNumber int) string {
	return "__multipart/" + uploadID + "/" + strconv.Itoa(partNumber)
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

	// Pick a backend with available quota (estimate 0 bytes since final size is unknown)
	backendName, err := mp.parent.selectWriteTarget(ctx, span, operation, 0)
	if err != nil {
		return "", "", err
	}

	uploadID := GenerateUploadID()
	if err := mp.parent.stores.Multipart.CreateMultipartUpload(ctx, uploadID, key, backendName, contentType, metadata); err != nil {
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
		telemetry.AttrUploadID.String(uploadID),
		telemetry.AttrPartNumber.Int(partNumber),
	)
	defer span.End()

	if partNumber < 1 || partNumber > 10000 {
		err := &core.S3Error{StatusCode: 400, Code: "InvalidArgument", Message: "Part number must be between 1 and 10000"}
		span.SetStatus(codes.Error, err.Message)
		return "", err
	}

	mu, err := mp.parent.stores.Multipart.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return "", mp.classifyWriteError(span, operation, err)
	}

	be, err := mp.getBackend(mu.BackendName)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	// Check usage limits before uploading
	if !mp.usage.WithinLimits(mu.BackendName, 1, 0, size) {
		span.SetStatus(codes.Error, "usage limits exceeded")
		return "", core.ErrInsufficientStorage
	}

	// Encrypt if enabled
	var enc *core.EncryptionMeta
	uploadBody := body
	uploadSize := size
	if mp.encryptor != nil {
		var encErr error
		uploadBody, uploadSize, enc, encErr = encryptBody(ctx, mp.encryptor, body, size)
		if encErr != nil {
			span.SetStatus(codes.Error, encErr.Error())
			return "", fmt.Errorf("encrypt part: %w", encErr)
		}
	}

	// Store part under a temp key
	partKey := multipartPartKey(uploadID, partNumber)
	bctx, bcancel := mp.withTimeout(ctx)
	defer bcancel()
	etag, err := be.PutObject(bctx, partKey, uploadBody, uploadSize, "application/octet-stream", nil)
	if err != nil {
		mp.usage.Record(mu.BackendName, 1, 0, 0) // API call was made even on failure
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("failed to upload part: %w", err)
	}

	if err := mp.parent.stores.Multipart.RecordPart(ctx, uploadID, partNumber, etag, uploadSize, enc); err != nil {
		slog.ErrorContext(ctx, "recordPart failed, cleaning up part object",
			"upload_id", uploadID, "part", partNumber, "error", err)
		// Account for both API calls the failure path made: the PUT that
		// succeeded against the backend (the success-path Record at the
		// bottom of this function only runs when we return nil) and the
		// cleanup DELETE about to run.
		mp.usage.Record(mu.BackendName, 1, 0, 0) // PUT
		delErr := mp.deleteWithTimeout(ctx, be, partKey)
		mp.usage.Record(mu.BackendName, 1, 0, 0) // cleanup DELETE
		if delErr != nil {
			slog.ErrorContext(ctx, "failed to clean up orphaned part object",
				"key", partKey, "error", delErr)
			mp.parent.enqueueCleanup(ctx, mu.BackendName, partKey, "orphan_part_record_failed", uploadSize)
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

// CompleteMultipartUpload reassembles parts into the final object.
// Downloads each part, concatenates them into a single upload, cleans up
// temp keys, and records the final object location with quota tracking.
func (mp *MultipartManager) CompleteMultipartUpload(ctx context.Context, uploadID string, partNumbers []int) (string, error) {
	const operation = "CompleteMultipartUpload"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		telemetry.AttrUploadID.String(uploadID),
	)
	defer span.End()

	mu, err := mp.parent.stores.Multipart.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return "", mp.classifyWriteError(span, operation, err)
	}
	be, err := mp.getBackend(mu.BackendName)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	parts, err := mp.collectRequestedParts(ctx, span, uploadID, partNumbers)
	if err != nil {
		return "", err
	}

	totalPlaintextSize, anyEncrypted := sumPlaintextSize(parts)

	pr, pipeCancel := mp.streamPartsThroughPipe(ctx, be, uploadID, parts)
	defer pipeCancel()

	uploadBody, uploadSize, enc, err := mp.buildAssembledUpload(ctx, span, pr, totalPlaintextSize, anyEncrypted)
	if err != nil {
		return "", err
	}

	// Streamed from pipe; deadline governed by the caller's context.
	etag, err := be.PutObject(ctx, mu.ObjectKey, uploadBody, uploadSize, mu.ContentType, mu.Metadata)
	if err != nil {
		pipeCancel()
		pr.Close()
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("failed to upload final object: %w", err)
	}
	pr.Close()

	if err := mp.parent.recordObjectOrCleanup(ctx, span, be, mu.ObjectKey, mu.BackendName, uploadSize, enc); err != nil {
		return "", err
	}

	for _, part := range parts {
		partKey := multipartPartKey(uploadID, part.PartNumber)
		mp.parent.deleteOrEnqueue(ctx, be, mu.BackendName, partKey, "complete_part_cleanup", part.SizeBytes)
	}
	if err := mp.parent.stores.Multipart.DeleteMultipartUpload(ctx, uploadID); err != nil {
		span.RecordError(err)
	}

	mp.recordOperation(operation, mu.BackendName, start, nil)
	mp.usage.Record(mu.BackendName, int64(2*len(parts)+1), 0, uploadSize)

	audit.Log(ctx, "storage.CompleteMultipartUpload",
		slog.String("key", mu.ObjectKey),
		slog.String("backend", mu.BackendName),
		slog.String("upload_id", uploadID),
		slog.Int64("total_size", totalPlaintextSize),
		slog.Int("parts_count", len(parts)),
	)
	if event.Emit != nil {
		bucket, userKey := internalkey.Split(mu.ObjectKey)
		event.Emit(event.Event{
			Type:    event.ObjectCreatedCompleteMultipartUpload,
			Subject: userKey,
			Data: map[string]any{
				"bucket":      bucket,
				"key":         userKey,
				"backend":     mu.BackendName,
				"size":        totalPlaintextSize,
				"parts_count": len(parts),
				"upload_id":   uploadID,
				"request_id":  audit.RequestID(ctx),
			},
		})
	}
	if mp.objectCache != nil {
		mp.objectCache.Invalidate(mu.ObjectKey)
	}

	span.SetStatus(codes.Ok, "")
	return etag, nil
}

// collectRequestedParts loads every part for uploadID, validates that all
// requested part numbers were uploaded, then returns the requested
// subset sorted in part-number order ready for assembly.
func (mp *MultipartManager) collectRequestedParts(ctx context.Context, span trace.Span, uploadID string, partNumbers []int) ([]core.MultipartPart, error) {
	allParts, err := mp.parent.stores.Multipart.GetParts(ctx, uploadID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	uploaded := make(map[int]bool, len(allParts))
	for _, p := range allParts {
		uploaded[p.PartNumber] = true
	}
	var missing []int
	for _, pn := range partNumbers {
		if !uploaded[pn] {
			missing = append(missing, pn)
		}
	}
	if len(missing) > 0 {
		msg := "parts not uploaded: " + formatPartNumbers(missing)
		span.SetStatus(codes.Error, msg)
		return nil, &core.S3Error{StatusCode: 400, Code: "InvalidPart", Message: msg}
	}

	requested := make(map[int]bool, len(partNumbers))
	for _, pn := range partNumbers {
		requested[pn] = true
	}
	var parts []core.MultipartPart
	for _, p := range allParts {
		if requested[p.PartNumber] {
			parts = append(parts, p)
		}
	}
	slices.SortFunc(parts, func(a, b core.MultipartPart) int {
		return a.PartNumber - b.PartNumber
	})
	return parts, nil
}

// sumPlaintextSize returns the total plaintext byte count across parts
// and whether any part was uploaded encrypted. Encrypted parts contribute
// PlaintextSize; unencrypted parts contribute SizeBytes.
func sumPlaintextSize(parts []core.MultipartPart) (int64, bool) {
	var total int64
	anyEncrypted := false
	for _, part := range parts {
		if part.Encrypted {
			total += part.PlaintextSize
			anyEncrypted = true
		} else {
			total += part.SizeBytes
		}
	}
	return total, anyEncrypted
}

// buildAssembledUpload prepares the request body sent to the backend
// during assembly. When the orchestrator encryptor is configured, the
// pipe is wrapped in encryptBody so the assembled object lands as a
// single ciphertext with unified chunk boundaries; otherwise the pipe is
// uploaded verbatim. anyEncrypted is informational - inline decryption
// already runs in streamPartsThroughPipe so the pipe always emits
// plaintext.
func (mp *MultipartManager) buildAssembledUpload(
	ctx context.Context,
	span trace.Span,
	pr io.Reader,
	totalPlaintextSize int64,
	anyEncrypted bool,
) (io.Reader, int64, *core.EncryptionMeta, error) {
	_ = anyEncrypted
	if mp.encryptor == nil {
		return pr, totalPlaintextSize, nil, nil
	}
	uploadBody, uploadSize, enc, encErr := encryptBody(ctx, mp.encryptor, pr, totalPlaintextSize)
	if encErr != nil {
		span.SetStatus(codes.Error, encErr.Error())
		return nil, 0, nil, fmt.Errorf("encrypt final object: %w", encErr)
	}
	return uploadBody, uploadSize, enc, nil
}

// AbortMultipartUpload cleans up an in-progress multipart upload, removing
// all part objects from the backend and the upload records from the database.
func (mp *MultipartManager) AbortMultipartUpload(ctx context.Context, uploadID string) error {
	const operation = "AbortMultipartUpload"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		telemetry.AttrUploadID.String(uploadID),
	)
	defer span.End()

	mu, err := mp.parent.stores.Multipart.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return mp.classifyWriteError(span, operation, err)
	}

	be, err := mp.getBackend(mu.BackendName)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	parts, err := mp.parent.stores.Multipart.GetParts(ctx, uploadID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return fmt.Errorf("failed to get parts for abort: %w", err)
	}

	// Delete part objects from backend
	for _, part := range parts {
		partKey := multipartPartKey(uploadID, part.PartNumber)
		mp.parent.deleteOrEnqueue(ctx, be, mu.BackendName, partKey, "abort_part_cleanup", part.SizeBytes)
	}

	// Delete multipart records from database
	if err := mp.parent.stores.Multipart.DeleteMultipartUpload(ctx, uploadID); err != nil {
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
func (mp *MultipartManager) ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]core.MultipartUpload, error) {
	return mp.parent.stores.Multipart.ListMultipartUploads(ctx, prefix, maxUploads)
}

// GetParts returns all parts for a multipart upload.
func (mp *MultipartManager) GetParts(ctx context.Context, uploadID string) ([]core.MultipartPart, error) {
	return mp.parent.stores.Multipart.GetParts(ctx, uploadID)
}

// CleanupStaleMultipartUploads aborts multipart uploads older than the given
// duration. Run periodically to prevent quota leaks from abandoned uploads.
func (mp *MultipartManager) CleanupStaleMultipartUploads(ctx context.Context, olderThan time.Duration) {
	uploads, err := mp.parent.stores.Multipart.GetStaleMultipartUploads(ctx, olderThan)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get stale multipart uploads", "error", err)
		return
	}

	cleaned := 0
	for _, mu := range uploads {
		slog.InfoContext(ctx, "cleaning up stale multipart upload", "upload_id", mu.UploadID, "key", mu.ObjectKey)
		if err := mp.AbortMultipartUpload(ctx, mu.UploadID); err != nil {
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
	uploads, err := mp.parent.stores.Multipart.GetMultipartUploadsByBackend(ctx, backendName)
	if err != nil {
		slog.ErrorContext(ctx, "Drain: failed to list multipart uploads", "backend", backendName, "error", err)
		return
	}

	for _, mu := range uploads {
		slog.InfoContext(ctx, "Drain: aborting multipart upload", "upload_id", mu.UploadID, "key", mu.ObjectKey)
		if err := mp.AbortMultipartUpload(ctx, mu.UploadID); err != nil {
			slog.ErrorContext(ctx, "Drain: failed to abort multipart upload",
				"upload_id", mu.UploadID, "error", err)
		}
	}
}

// -------------------------------------------------------------------------
// PART STREAMING
// -------------------------------------------------------------------------

// streamPartsThroughPipe spawns a goroutine that reads each part in order,
// decrypts encrypted parts inline so the pipe carries plaintext, and writes
// the concatenated stream to the returned reader. The caller must invoke
// the returned cancel func to stop in-flight backend reads when assembly
// fails downstream (e.g. the final PutObject errors out).
func (mp *MultipartManager) streamPartsThroughPipe(
	ctx context.Context,
	be s3be.ObjectBackend,
	uploadID string,
	parts []core.MultipartPart,
) (*io.PipeReader, context.CancelFunc) {
	pr, pw := io.Pipe()
	pipeCtx, pipeCancel := context.WithCancel(ctx)

	go func() {
		bw := bufpool.GetWriter(pw)
		defer func() {
			if r := recover(); r != nil {
				pw.CloseWithError(fmt.Errorf("multipart assembly panic: %v", r))
			}
			bufpool.PutWriter(bw)
			_ = pw.Close()
		}()
		for i := range parts {
			if err := mp.streamOnePart(pipeCtx, be, bw, uploadID, &parts[i]); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		if err := bw.Flush(); err != nil {
			pw.CloseWithError(fmt.Errorf("failed to flush multipart stream: %w", err))
		}
	}()

	return pr, pipeCancel
}

// streamOnePart fetches one part from the backend, decrypts it when the
// part was stored encrypted, and copies the plaintext into bw. Closes the
// backend response body and the per-call timeout context before returning.
// Errors are wrapped with the part number so the assembly failure message
// identifies which part failed.
func (mp *MultipartManager) streamOnePart(
	ctx context.Context,
	be s3be.ObjectBackend,
	bw io.Writer,
	uploadID string,
	part *core.MultipartPart,
) error {
	partKey := multipartPartKey(uploadID, part.PartNumber)
	bctx, bcancel := mp.withTimeout(ctx)
	defer bcancel()

	result, err := be.GetObject(bctx, partKey, "")
	if err != nil {
		return fmt.Errorf("failed to read part %d: %w", part.PartNumber, err)
	}
	defer func() { _ = result.Body.Close() }()

	src := io.Reader(result.Body)
	if part.Encrypted && mp.encryptor != nil {
		_, wrappedDEK, unpackErr := encryption.UnpackKeyData(part.EncryptionKey)
		if unpackErr != nil {
			telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt", "unpack_failed").Inc()
			return fmt.Errorf("unpack part %d key: %w", part.PartNumber, unpackErr)
		}
		decrypted, decErr := mp.encryptor.Decrypt(ctx, result.Body, wrappedDEK, part.KeyID)
		if decErr != nil {
			telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt", "decrypt_failed").Inc()
			return fmt.Errorf("decrypt part %d: %w", part.PartNumber, decErr)
		}
		telemetry.EncryptionOpsTotal.WithLabelValues("decrypt").Inc()
		src = decrypted
	}

	if _, err := bufpool.Copy(bw, src); err != nil {
		return fmt.Errorf("failed to stream part %d: %w", part.PartNumber, err)
	}
	return nil
}

// formatPartNumbers formats a slice of part numbers for error messages.
func formatPartNumbers(parts []int) string {
	s := make([]string, len(parts))
	for i, pn := range parts {
		s[i] = strconv.Itoa(pn)
	}
	return strings.Join(s, ", ")
}

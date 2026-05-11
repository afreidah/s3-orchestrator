// -------------------------------------------------------------------------------
// Multipart Manager - Complete and Assembly
//
// Author: Alex Freidah
//
// CompleteMultipartUpload runs under a per-uploadID advisory lock so two
// concurrent Complete calls for the same upload cannot both stream parts
// and PUT the assembled object on top of each other. The body fetches and
// validates parts, streams them through an io.Pipe with inline part-level
// decryption, re-encrypts the assembled stream under the shared upload
// DEK when encryption is configured, PUTs the final object, and records
// the location with quota tracking. Cleanup of part objects and the
// upload row runs from a defer so a failed assembly PUT or metadata
// commit still drops the temp objects through the cleanup queue.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
)

// CompleteMultipartUpload reassembles parts into the final object.
// Downloads each part, concatenates them into a single upload, cleans up
// temp keys, and records the final object location with quota tracking.
//
// The body runs under a session-scoped advisory lock keyed by uploadID
// so two concurrent Complete calls for the same upload cannot both
// stream parts and PUT the assembled object on top of each other (which
// would leave the backend bytes and the metadata row pointing at
// different writers). When the lock is contended the second caller
// fails fast with a 409 OperationAborted so the client can decide
// whether to retry or abort.
func (mp *MultipartManager) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int) (string, error) {
	const operation = "CompleteMultipartUpload"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
		telemetry.AttrUploadID.String(uploadID),
	)
	defer span.End()

	mu, err := mp.fetchScopedUpload(ctx, span, bucket, key, uploadID, operation)
	if err != nil {
		return "", err
	}
	_ = mu // CompleteMultipartUpload's locked path re-fetches under the advisory lock.

	var etag string
	acquired, err := mp.stores.WithAdvisoryLock(ctx, uploadIDLockID(uploadID), func(ctx context.Context) error {
		var inner error
		etag, inner = mp.completeMultipartUploadLocked(ctx, span, operation, uploadID, partNumbers, start)
		return inner
	})
	if err != nil {
		return "", err
	}
	if !acquired {
		span.SetStatus(codes.Error, "another CompleteMultipartUpload in flight")
		return "", &core.S3Error{
			StatusCode: 409,
			Code:       "OperationAborted",
			Message:    "Another CompleteMultipartUpload is already in progress for this upload",
		}
	}
	return etag, nil
}

// completeMultipartUploadLocked runs the actual assembly under the
// advisory lock acquired by CompleteMultipartUpload. Cleanup of part
// objects and the multipart_uploads metadata row happens via a
// deferred closure once parts have been resolved, so a failed assembly
// PUT or recordObject still drops the part objects through
// deleteOrEnqueue (and accounts for them in cleanup_queue / orphan
// bytes) instead of leaving them visible only to the periodic
// stale-multipart sweeper.
func (mp *MultipartManager) completeMultipartUploadLocked(
	ctx context.Context,
	span trace.Span,
	operation, uploadID string,
	partNumbers []int,
	start time.Time,
) (string, error) {
	mu, err := mp.stores.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return "", mp.classifyWriteError(span, operation, err)
	}
	be, err := mp.GetBackend(mu.BackendName)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	parts, err := mp.collectRequestedParts(ctx, span, uploadID, partNumbers)
	if err != nil {
		return "", err
	}

	defer mp.cleanupCompletedUpload(ctx, span, be, mu, uploadID, parts)

	totalPlaintextSize := sumPlaintextSize(parts)

	pr, pipeCancel := mp.streamPartsThroughPipe(ctx, be, uploadID, parts)
	defer pipeCancel()

	uploadBody, uploadSize, enc, err := mp.buildAssembledUpload(ctx, span, mu, pr, totalPlaintextSize)
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

	if err := mp.coord.recordObjectOrCleanup(ctx, span, be, mu.ObjectKey, mu.BackendName, uploadSize, enc); err != nil {
		return "", err
	}

	mp.recordOperation(operation, mu.BackendName, start, nil)
	// N part GETs + 1 assembled PUT. The N cleanup DELETEs of the part
	// temp keys go through DeleteOrEnqueue, which records them itself.
	mp.usage.Record(mu.BackendName, int64(len(parts)+1), 0, uploadSize)

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

// cleanupCompletedUpload removes part objects and the multipart_uploads
// metadata row for an upload whose Complete attempt has finished
// (success or failure). Runs from a defer so a failed assembly PUT or
// recordObject still drops the part objects via deleteOrEnqueue,
// keeping the cleanup queue and orphan-bytes accounting accurate
// instead of relying on the periodic stale-multipart sweeper. Also
// evicts the upload's unwrapped DEK from the per-instance cache so
// abandoned-upload memory does not linger. Best effort: each step
// logs and continues so a single transient error cannot strand the
// rest of the cleanup.
func (mp *MultipartManager) cleanupCompletedUpload(ctx context.Context, span trace.Span, be s3be.ObjectBackend, mu *core.MultipartUpload, uploadID string, parts []core.MultipartPart) {
	for _, part := range parts {
		partKey := multipartPartKey(uploadID, part.PartNumber)
		mp.coord.DeleteOrEnqueue(ctx, be, mu.BackendName, partKey, "complete_part_cleanup", part.SizeBytes)
	}
	if err := mp.stores.DeleteMultipartUpload(ctx, uploadID); err != nil {
		span.RecordError(err)
	}
	mp.forgetUploadDEK(uploadID)
}

// sumPlaintextSize returns the total plaintext byte count across parts.
// Encrypted parts contribute PlaintextSize; unencrypted parts contribute
// SizeBytes.
func sumPlaintextSize(parts []core.MultipartPart) int64 {
	var total int64
	for _, part := range parts {
		if part.Encrypted {
			total += part.PlaintextSize
		} else {
			total += part.SizeBytes
		}
	}
	return total
}

// buildAssembledUpload prepares the request body sent to the backend
// during assembly. When the orchestrator encryptor is configured, the
// pipe is wrapped in EncryptWithDEK using the upload-level DEK so the
// assembled object lands as a single ciphertext that shares its DEK
// with every part. Inline decryption already runs in
// streamPartsThroughPipe so the pipe always emits plaintext. mu is
// required when the encryptor is configured because the assembled
// object must reuse mu.EncryptionKey / mu.KeyID rather than wrapping a
// fresh DEK for the final write.
func (mp *MultipartManager) buildAssembledUpload(
	ctx context.Context,
	span trace.Span,
	mu *core.MultipartUpload,
	pr io.Reader,
	totalPlaintextSize int64,
) (io.Reader, int64, *core.EncryptionMeta, error) {
	if mp.encryptor == nil {
		return pr, totalPlaintextSize, nil, nil
	}
	out, ciphertextSize, enc, err := mp.encryptWithUploadDEK(ctx, mu, pr, totalPlaintextSize)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, 0, nil, fmt.Errorf("encrypt final object: %w", err)
	}
	return out, ciphertextSize, enc, nil
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
	bctx, bcancel := mp.WithTimeout(ctx)
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

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
	"hash/fnv"
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
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// MultipartManager handles the multipart upload lifecycle.
//
// dekCache holds the unwrapped per-upload DEK keyed by uploadID so an
// instance that handles many UploadPart calls for the same upload pays
// for the KeyProvider unwrap round-trip once. The cache lifetime is
// pegged to the multipart stale-upload sweep interval so an abandoned
// upload's DEK does not linger in memory beyond its server-side
// existence. Concurrent UploadPart calls on the same uploadID with a
// cold cache will each issue their own Unwrap; the design accepts that
// minor cold-start cost in exchange for not pulling in singleflight.
type MultipartManager struct {
	*backendCore
	parent      *BackendManager // set post-construction; routes write-path helpers to the parent's store fields
	encryptor   *encryption.Encryptor
	objectCache objcache.ObjectCache
	dekCache    *syncutil.TTLCache[string, []byte]
}

// NewMultipartManager creates a MultipartManager sharing the given core
// infrastructure and optional encryptor. The caller wires the parent
// BackendManager pointer after construction.
func NewMultipartManager(core *backendCore, encryptor *encryption.Encryptor, objectCache objcache.ObjectCache, dekCacheTTL time.Duration) *MultipartManager {
	return &MultipartManager{
		backendCore: core,
		encryptor:   encryptor,
		objectCache: objectCache,
		dekCache:    syncutil.NewTTLCache[string, []byte](dekCacheTTL),
	}
}

// unwrapUploadDEK returns the unwrapped DEK for a multipart upload,
// caching the result for the lifetime of the upload so subsequent
// UploadParts on this instance do not re-issue the KeyProvider round-
// trip. Returns the unwrapped DEK and the wrapped form (for write-path
// metadata that needs the wrapped value).
func (mp *MultipartManager) unwrapUploadDEK(ctx context.Context, mu *core.MultipartUpload) (dek, wrappedDEK []byte, baseNonce []byte, err error) {
	if !mu.Encrypted || len(mu.EncryptionKey) == 0 {
		return nil, nil, nil, fmt.Errorf("upload %s carries no encryption metadata", mu.UploadID)
	}
	baseNonce, wrappedDEK, err = encryption.UnpackKeyData(mu.EncryptionKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unpack upload encryption metadata: %w", err)
	}
	if cached, ok := mp.dekCache.Get(mu.UploadID); ok {
		return cached, wrappedDEK, baseNonce, nil
	}
	unwrapped, err := mp.encryptor.Provider().UnwrapDEK(ctx, wrappedDEK, mu.KeyID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unwrap upload DEK: %w", err)
	}
	mp.dekCache.Set(mu.UploadID, unwrapped)
	return unwrapped, wrappedDEK, baseNonce, nil
}

// forgetUploadDEK drops a cached unwrapped DEK so the upload's DEK
// stops occupying memory once the upload has reached a terminal state
// (Complete/Abort/expiry).
func (mp *MultipartManager) forgetUploadDEK(uploadID string) {
	mp.dekCache.Delete(uploadID)
}

// multipartPartKey returns the temporary object key for a multipart part.
func multipartPartKey(uploadID string, partNumber int) string {
	return "__multipart/" + uploadID + "/" + strconv.Itoa(partNumber)
}

// -------------------------------------------------------------------------
// MULTIPART UPLOAD OPERATIONS
// -------------------------------------------------------------------------

// CreateMultipartUpload initiates a multipart upload by selecting a backend
// with available quota and recording the upload in the database. When
// proxy-side encryption is configured, a single DEK is wrapped once
// here and persisted on the multipart_uploads row so every subsequent
// UploadPart can reuse it without paying its own KeyProvider
// round-trip (this is the shared-DEK invariant CompleteMultipartUpload
// also depends on).
func (mp *MultipartManager) CreateMultipartUpload(ctx context.Context, key, contentType string, metadata map[string]string) (string, string, error) {
	const operation = "CreateMultipartUpload"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()

	// Pick a backend with available quota (estimate 0 bytes since final size is unknown)
	backendName, err := mp.parent.selectWriteTarget(ctx, span, operation, 0)
	if err != nil {
		return "", "", err
	}

	uploadID := GenerateUploadID()

	// Generate the upload-level DEK now so every UploadPart and the
	// assembled object's CompleteMultipartUpload share one wrapped DEK.
	// The packed format mirrors object_locations.encryption_key:
	// PackKeyData(baseNonce, wrappedDEK). For the upload row the base
	// nonce is intentionally zero - the upload row never directly
	// produces ciphertext; per-part baseNonces live on each
	// multipart_parts row and the assembled object stores its own
	// baseNonce in object_locations.encryption_key.
	var (
		encryptionKey []byte
		keyID         string
	)
	if mp.encryptor != nil {
		_, wrappedDEK, kid, kerr := mp.encryptor.GenerateAndWrapDEK(ctx)
		if kerr != nil {
			span.SetStatus(codes.Error, kerr.Error())
			return "", "", fmt.Errorf("wrap upload DEK: %w", kerr)
		}
		encryptionKey = encryption.PackKeyData(make([]byte, encryption.NonceSize), wrappedDEK)
		keyID = kid
	}

	if err := mp.parent.stores.Multipart.CreateMultipartUpload(ctx, &core.CreateMultipartUploadParams{
		UploadID:      uploadID,
		ObjectKey:     key,
		BackendName:   backendName,
		ContentType:   contentType,
		Metadata:      metadata,
		EncryptionKey: encryptionKey,
		KeyID:         keyID,
	}); err != nil {
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

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
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

	// Encrypt if enabled. The upload-level DEK lives on the
	// multipart_uploads row and is shared by every part; we unwrap it
	// once per instance via the dekCache and reuse it here so this
	// path costs zero KeyProvider round-trips after the first part.
	// EncryptWithDEK generates a fresh per-part baseNonce internally
	// so the AES-GCM (key, nonce) uniqueness invariant holds across
	// every part of the upload.
	var enc *core.EncryptionMeta
	uploadBody := body
	uploadSize := size
	if mp.encryptor != nil && mu.Encrypted {
		dek, wrappedDEK, _, derr := mp.unwrapUploadDEK(ctx, mu)
		if derr != nil {
			span.SetStatus(codes.Error, derr.Error())
			return "", derr
		}
		result, err := mp.encryptor.EncryptWithDEK(body, size, dek, wrappedDEK, mu.KeyID)
		if err != nil {
			telemetry.EncryptionErrorsTotal.WithLabelValues("encrypt", "encrypt_failed").Inc()
			span.SetStatus(codes.Error, err.Error())
			return "", fmt.Errorf("encrypt part: %w", err)
		}
		telemetry.EncryptionOpsTotal.WithLabelValues("encrypt").Inc()
		uploadBody = result.Body
		uploadSize = result.CiphertextSize
		enc = &core.EncryptionMeta{
			Encrypted:     true,
			EncryptionKey: encryption.PackKeyData(result.BaseNonce, result.WrappedDEK),
			KeyID:         result.KeyID,
			PlaintextSize: size,
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

// uploadIDLockNamespace is OR'd into every multipart-upload advisory
// lock ID so per-uploadID locks live above 2^62 and cannot collide
// with the small reserved service lock IDs in core/locks.go.
const uploadIDLockNamespace int64 = 1 << 62

// uploadIDLockID derives a stable advisory-lock ID from a multipart
// upload ID. FNV-64a is fast and uniform; the namespace bit keeps the
// per-key range disjoint from the service lock IDs (1001-1011 today).
func uploadIDLockID(uploadID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(uploadID))
	return uploadIDLockNamespace | int64(h.Sum64()&((1<<62)-1))
}

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
func (mp *MultipartManager) CompleteMultipartUpload(ctx context.Context, uploadID string, partNumbers []int) (string, error) {
	const operation = "CompleteMultipartUpload"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
		telemetry.AttrUploadID.String(uploadID),
	)
	defer span.End()

	var etag string
	acquired, err := mp.parent.stores.Lock.WithAdvisoryLock(ctx, uploadIDLockID(uploadID), func(ctx context.Context) error {
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

	defer mp.cleanupCompletedUpload(ctx, span, be, mu, uploadID, parts)

	totalPlaintextSize, anyEncrypted := sumPlaintextSize(parts)

	pr, pipeCancel := mp.streamPartsThroughPipe(ctx, be, uploadID, parts)
	defer pipeCancel()

	uploadBody, uploadSize, enc, err := mp.buildAssembledUpload(ctx, span, mu, pr, totalPlaintextSize, anyEncrypted)
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
		mp.parent.deleteOrEnqueue(ctx, be, mu.BackendName, partKey, "complete_part_cleanup", part.SizeBytes)
	}
	if err := mp.parent.stores.Multipart.DeleteMultipartUpload(ctx, uploadID); err != nil {
		span.RecordError(err)
	}
	mp.forgetUploadDEK(uploadID)
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
// pipe is wrapped in EncryptWithDEK using the upload-level DEK so the
// assembled object lands as a single ciphertext that shares its DEK
// with every part. anyEncrypted is informational - inline decryption
// already runs in streamPartsThroughPipe so the pipe always emits
// plaintext. mu is required when the encryptor is configured because
// the assembled object must reuse mu.EncryptionKey / mu.KeyID rather
// than wrapping a fresh DEK for the final write.
func (mp *MultipartManager) buildAssembledUpload(
	ctx context.Context,
	span trace.Span,
	mu *core.MultipartUpload,
	pr io.Reader,
	totalPlaintextSize int64,
	anyEncrypted bool,
) (io.Reader, int64, *core.EncryptionMeta, error) {
	_ = ctx
	_ = anyEncrypted
	if mp.encryptor == nil {
		return pr, totalPlaintextSize, nil, nil
	}
	dek, wrappedDEK, _, derr := mp.unwrapUploadDEK(ctx, mu)
	if derr != nil {
		span.SetStatus(codes.Error, derr.Error())
		return nil, 0, nil, derr
	}
	result, err := mp.encryptor.EncryptWithDEK(pr, totalPlaintextSize, dek, wrappedDEK, mu.KeyID)
	if err != nil {
		telemetry.EncryptionErrorsTotal.WithLabelValues("encrypt", "encrypt_failed").Inc()
		span.SetStatus(codes.Error, err.Error())
		return nil, 0, nil, fmt.Errorf("encrypt final object: %w", err)
	}
	telemetry.EncryptionOpsTotal.WithLabelValues("encrypt").Inc()
	return result.Body, result.CiphertextSize, &core.EncryptionMeta{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(result.BaseNonce, result.WrappedDEK),
		KeyID:         result.KeyID,
		PlaintextSize: totalPlaintextSize,
	}, nil
}

// AbortMultipartUpload cleans up an in-progress multipart upload, removing
// all part objects from the backend and the upload records from the database.
func (mp *MultipartManager) AbortMultipartUpload(ctx context.Context, uploadID string) error {
	const operation = "AbortMultipartUpload"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
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

	// Evict the upload's unwrapped DEK so the cache does not hold
	// onto an aborted upload's key material.
	mp.forgetUploadDEK(uploadID)

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
	for i := range uploads {
		mu := &uploads[i]
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

	for i := range uploads {
		mu := &uploads[i]
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

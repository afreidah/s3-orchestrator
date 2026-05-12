// -------------------------------------------------------------------------------
// Multipart Manager - Type, Constructor, and Upload Entry Points
//
// Author: Alex Freidah
//
// Owns the MultipartManager type and the entry points clients hit first:
// CreateMultipartUpload (which wraps the upload-level DEK once so every
// subsequent UploadPart and the assembled object reuse it), UploadPart,
// and the simple pass-through accessors ListMultipartUploads and
// GetParts. The complete/abort/cleanup/encryption/lock helpers live in
// sibling multipart_*.go files; this file is the public surface plus
// type declarations.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/codes"

	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
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
	coord       *writeCoordinator  // write-path helpers shared with BackendManager and ObjectManager
	stores      core.MetadataStore // direct store access for multipart row/part operations and WithAdvisoryLock
	encryptor   *encryption.Encryptor
	objectCache objcache.ObjectCache
	dekCache    *syncutil.TTLCache[string, []byte]
}

// NewMultipartManager creates a MultipartManager sharing the given core
// infrastructure and write coordinator. All dependencies must be
// non-nil; nothing is patched in post-construction.
func NewMultipartManager(core *backendCore, coord *writeCoordinator, stores core.MetadataStore, encryptor *encryption.Encryptor, objectCache objcache.ObjectCache, dekCacheTTL time.Duration) *MultipartManager {
	return &MultipartManager{
		backendCore: core,
		coord:       coord,
		stores:      stores,
		encryptor:   encryptor,
		objectCache: objectCache,
		dekCache:    syncutil.NewTTLCache[string, []byte](dekCacheTTL),
	}
}

// invalidateCache removes a key from the object data cache if caching is enabled.
func (mp *MultipartManager) invalidateCache(key string) {
	if mp.objectCache != nil {
		mp.objectCache.Invalidate(key)
	}
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
	backendName, err := mp.coord.selectWriteTarget(ctx, span, operation, 0)
	if err != nil {
		return "", "", err
	}

	uploadID := audit.NewID()

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
			observe.RecordSpanError(span, kerr)
			return "", "", fmt.Errorf("wrap upload DEK: %w", kerr)
		}
		encryptionKey = encryption.PackKeyData(make([]byte, encryption.NonceSize), wrappedDEK)
		keyID = kid
	}

	if err := mp.stores.CreateMultipartUpload(ctx, &core.CreateMultipartUploadParams{
		UploadID:      uploadID,
		ObjectKey:     key,
		BackendName:   backendName,
		ContentType:   contentType,
		Metadata:      metadata,
		EncryptionKey: encryptionKey,
		KeyID:         keyID,
	}); err != nil {
		observe.RecordSpanError(span, err)
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
func (mp *MultipartManager) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, body io.Reader, size int64) (string, error) {
	const operation = "UploadPart"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
		telemetry.AttrUploadID.String(uploadID),
		telemetry.AttrPartNumber.Int(partNumber),
	)
	defer span.End()

	if partNumber < 1 || partNumber > 10000 {
		err := &core.S3Error{StatusCode: 400, Code: "InvalidArgument", Message: "Part number must be between 1 and 10000"}
		observe.MarkSpanError(span, err.Message)
		return "", err
	}

	mu, err := mp.fetchScopedUpload(ctx, span, bucket, key, uploadID, operation)
	if err != nil {
		return "", err
	}

	be, err := mp.GetBackend(mu.BackendName)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}

	// Check usage limits before uploading
	if !mp.usage.WithinLimits(mu.BackendName, 1, 0, size) {
		observe.MarkSpanError(span, "usage limits exceeded")
		return "", core.ErrInsufficientStorage
	}

	uploadBody, uploadSize, enc, err := mp.prepareUploadPartBody(ctx, mu, body, size)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}

	// Store part under a temp key
	partKey := multipartPartKey(uploadID, partNumber)
	bctx, bcancel := mp.WithTimeout(ctx)
	defer bcancel()
	etag, err := be.PutObject(bctx, partKey, uploadBody, uploadSize, "application/octet-stream", nil)
	if err != nil {
		mp.usage.Record(mu.BackendName, 1, 0, 0) // API call was made even on failure
		observe.RecordSpanError(span, err)
		return "", fmt.Errorf("failed to upload part: %w", err)
	}

	if err := mp.stores.RecordPart(ctx, uploadID, partNumber, etag, uploadSize, enc); err != nil {
		mp.Log().ErrorContext(ctx, "recordPart failed, cleaning up part object",
			"upload_id", uploadID, "part", partNumber, "error", err)
		mp.coord.recoverFromRecordFailure(ctx, be, mu.BackendName, partKey, "orphan_part_record_failed", uploadSize)
		observe.RecordSpanError(span, err)
		return "", fmt.Errorf("failed to record part: %w", err)
	}

	mp.recordOperation(operation, mu.BackendName, start, nil)
	mp.usage.Record(mu.BackendName, 1, 0, size)
	span.SetStatus(codes.Ok, "")
	return etag, nil
}

// -------------------------------------------------------------------------
// READ-ONLY ACCESSORS
// -------------------------------------------------------------------------

// ListMultipartUploads returns active multipart uploads matching the given
// prefix, up to maxUploads results. Pass-through to the metadata store.
func (mp *MultipartManager) ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]core.MultipartUpload, error) {
	return mp.stores.ListMultipartUploads(ctx, prefix, maxUploads)
}

// GetParts returns all parts for a multipart upload.
func (mp *MultipartManager) GetParts(ctx context.Context, bucket, key, uploadID string) ([]core.MultipartPart, error) {
	const operation = "GetParts"
	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
		telemetry.AttrUploadID.String(uploadID),
	)
	defer span.End()
	if _, err := mp.fetchScopedUpload(ctx, span, bucket, key, uploadID, operation); err != nil {
		return nil, err
	}
	return mp.stores.GetParts(ctx, uploadID)
}

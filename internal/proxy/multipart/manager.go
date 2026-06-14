// -------------------------------------------------------------------------------
// Multipart Manager
//
// Author: Alex Freidah
//
// Manager owns the multipart-upload lifecycle: creation,
// per-part uploads, completion, abort/cleanup, the encryption helpers
// used by parts and the assembled object, the part/upload-row helpers
// shared across paths, and the advisory-lock ID derivation used by
// CompleteMultipartUpload.
// -------------------------------------------------------------------------------

package multipart

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	pobserve "github.com/afreidah/s3-orchestrator/internal/proxy/observe"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// spanPrefix is prepended to every OpenTelemetry span name this package
// creates so traces distinguish the manager layer ("Manager UploadPart")
// from the backend layer ("Backend UploadPart") in the same trace.
const spanPrefix = "Manager "

// Manager handles the multipart upload lifecycle.
//
// dekCache holds the unwrapped per-upload DEK keyed by uploadID so an
// instance that handles many UploadPart calls for the same upload pays
// for the KeyProvider unwrap round-trip once. The cache lifetime is
// pegged to the multipart stale-upload sweep interval so an abandoned
// upload's DEK does not linger in memory beyond its server-side
// existence. Concurrent UploadPart calls on the same uploadID with a
// cold cache will each issue their own Unwrap; the design accepts that
// minor cold-start cost in exchange for not pulling in singleflight.
// MultipartStores is the narrow persistence surface multipart needs: multipart
// row/part operations and the advisory lock used to serialize stale-upload
// sweeps. Declared locally so multipart does not pull in the full
// MetadataStore.
type MultipartStores interface {
	core.MultipartStore
	core.AdvisoryLocker
}

type Manager struct {
	core         MultipartRuntime       // infrastructure subset: backends, usage, timeout, error classification, metrics
	coord        *writepath.Coordinator // write-path helpers shared with BackendManager and ObjectManager
	stores       MultipartStores        // multipart row/part operations and WithAdvisoryLock
	encryptor    *encryption.Encryptor
	objectCache  objcache.ObjectCache
	dekCache     *syncutil.TTLCache[string, []byte]
	integrityCfg *syncutil.AtomicConfig[config.IntegrityConfig] // nil-safe; controls plaintext SHA-256 on Complete
	log          *slog.Logger
}

// New creates a Manager sharing the given core infrastructure and
// write coordinator. All dependencies must be non-nil; nothing is
// patched in post-construction. integrityCfg is nil-safe - when nil or
// disabled, CompleteMultipartUpload skips the plaintext-hash tee that
// populates content_hash on the recorded location. The
// component-scoped logger is built in the constructor body per the
// project's logging convention.
func New(deps *Deps) *Manager {
	must.NotNil("deps", deps)
	must.NotNil("Core", deps.Core)
	must.NotNil("Coord", deps.Coord)
	must.NotNil("Stores", deps.Stores)
	return &Manager{
		core:         deps.Core,
		coord:        deps.Coord,
		stores:       deps.Stores,
		encryptor:    deps.Encryptor,
		objectCache:  deps.ObjectCache,
		dekCache:     syncutil.NewTTLCache[string, []byte](deps.DEKCacheTTL),
		integrityCfg: deps.IntegrityCfg,
		log:          slog.Default().With(logfmt.Component("multipart")),
	}
}

// Deps groups the multipart manager's constructor parameters: backend
// runtime, shared write coordinator, store surface, optional encryption /
// object cache, the DEK-cache TTL, and the shared integrity config.
type Deps struct {
	Core         MultipartRuntime
	Coord        *writepath.Coordinator
	Stores       MultipartStores
	Encryptor    *encryption.Encryptor // nil when encryption is disabled
	ObjectCache  objcache.ObjectCache  // nil when object caching is disabled
	DEKCacheTTL  time.Duration
	IntegrityCfg *syncutil.AtomicConfig[config.IntegrityConfig]
}

// Close stops the per-upload DEK cache eviction loop.
func (mp *Manager) Close() {
	if mp.dekCache != nil {
		mp.dekCache.Close()
	}
}

// invalidateCache removes a key from the object data cache if caching is enabled.
func (mp *Manager) invalidateCache(key string) {
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
func (mp *Manager) CreateMultipartUpload(ctx context.Context, key, contentType string, metadata map[string]string) (string, string, error) {
	const operation = "CreateMultipartUpload"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, spanPrefix+operation,
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()

	// Pick a backend with available quota (estimate 0 bytes since final size is unknown)
	backendName, err := mp.coord.SelectWriteTarget(ctx, span, operation, 0)
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
	mp.core.Acct().Operation(operation, backendName, start, nil)

	pobserve.MultipartCreated(ctx, span, key, backendName, uploadID)
	return uploadID, backendName, nil
}

// UploadPart uploads a single part to the backend. Parts are stored under a
// temporary key prefix and reassembled on completion.
func (mp *Manager) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, body io.Reader, size int64) (string, error) {
	const operation = "UploadPart"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, spanPrefix+operation,
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

	be, err := mp.core.GetBackend(mu.BackendName)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}

	// Check usage limits before uploading
	if !mp.core.Usage().WithinLimits(mu.BackendName, 1, 0, size) {
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
	bctx, bcancel := mp.core.WithTimeout(ctx)
	defer bcancel()
	etag, err := be.PutObject(bctx, partKey, uploadBody, uploadSize, "application/octet-stream", nil)
	if err != nil {
		mp.core.Acct().APICall(mu.BackendName) // API call was made even on failure
		observe.RecordSpanError(span, err)
		return "", fmt.Errorf("failed to upload part: %w", err)
	}

	if err := mp.stores.RecordPart(ctx, uploadID, partNumber, etag, uploadSize, enc); err != nil {
		mp.log.ErrorContext(ctx, "recordPart failed, cleaning up part object",
			"upload_id", uploadID, "part", partNumber, "error", err)
		mp.coord.RecoverFromRecordFailure(ctx, be, mu.BackendName, partKey, "orphan_part_record_failed", uploadSize)
		observe.RecordSpanError(span, err)
		return "", fmt.Errorf("failed to record part: %w", err)
	}

	mp.core.Acct().PutSuccess(operation, mu.BackendName, size, start)
	pobserve.UploadPartCompleted(ctx, span, mu.ObjectKey, mu.BackendName, uploadID, partNumber, size)
	return etag, nil
}

// -------------------------------------------------------------------------
// READ-ONLY ACCESSORS
// -------------------------------------------------------------------------

// ListMultipartUploads returns active multipart uploads matching the given
// prefix, up to maxUploads results. Pass-through to the metadata store.
func (mp *Manager) ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]core.MultipartUpload, error) {
	return mp.stores.ListMultipartUploads(ctx, prefix, maxUploads)
}

// GetParts returns all parts for a multipart upload.
func (mp *Manager) GetParts(ctx context.Context, bucket, key, uploadID string) ([]core.MultipartPart, error) {
	const operation = "GetParts"
	ctx, span := telemetry.StartSpan(ctx, spanPrefix+operation,
		telemetry.AttrUploadID.String(uploadID),
	)
	defer span.End()
	if _, err := mp.fetchScopedUpload(ctx, span, bucket, key, uploadID, operation); err != nil {
		return nil, err
	}
	return mp.stores.GetParts(ctx, uploadID)
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

// UnwrapUploadDEK returns the unwrapped DEK for a multipart upload,
// caching the result for the lifetime of the upload so subsequent
// UploadParts on this instance do not re-issue the KeyProvider round-
// trip. Returns the unwrapped DEK and the wrapped form (for write-path
// metadata that needs the wrapped value).
func (mp *Manager) UnwrapUploadDEK(ctx context.Context, mu *core.MultipartUpload) (dek, wrappedDEK []byte, baseNonce []byte, err error) {
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
func (mp *Manager) forgetUploadDEK(uploadID string) {
	mp.dekCache.Delete(uploadID)
}

// encryptWithUploadDEK wraps body in EncryptWithDEK using the
// upload-level DEK from mu and returns the ciphertext reader, its
// ciphertext size, and the EncryptionMeta the caller should persist on
// the resulting part or object row. The caller is responsible for
// deciding whether to encrypt at all (the upload row may have been
// created unencrypted, or the encryptor may be unconfigured) and for
// wrapping the returned error with a call-site-specific context.
// Counters are incremented here so every successful encrypt and every
// failure path is observable from one place.
func (mp *Manager) encryptWithUploadDEK(ctx context.Context, mu *core.MultipartUpload, body io.Reader, size int64) (io.Reader, int64, *core.EncryptionMeta, error) {
	dek, wrappedDEK, _, err := mp.UnwrapUploadDEK(ctx, mu)
	if err != nil {
		return nil, 0, nil, err
	}
	result, err := mp.encryptor.EncryptWithDEK(body, size, dek, wrappedDEK, mu.KeyID)
	if err != nil {
		telemetry.EncryptionErrorsTotal.WithLabelValues("encrypt", "encrypt_failed").Inc()
		return nil, 0, nil, err
	}
	telemetry.EncryptionOpsTotal.WithLabelValues("encrypt").Inc()
	return result.Body, result.CiphertextSize, &core.EncryptionMeta{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(result.BaseNonce, result.WrappedDEK),
		KeyID:         result.KeyID,
		PlaintextSize: size,
	}, nil
}

// prepareUploadPartBody returns the body, size, and encryption metadata
// the backend PUT should use for one part. When the upload row is flagged
// as encrypted and the manager has an encryptor configured, the per-part
// body is wrapped with EncryptWithDEK using the upload-level DEK from the
// dekCache (zero KeyProvider round-trips after the first unwrap). The
// AES-GCM (key, nonce) uniqueness invariant holds across every part
// because EncryptWithDEK generates a fresh per-part base nonce internally.
// When encryption is disabled or the upload was created unencrypted, the
// inputs are returned unchanged with a nil EncryptionMeta.
func (mp *Manager) prepareUploadPartBody(ctx context.Context, mu *core.MultipartUpload, body io.Reader, size int64) (io.Reader, int64, *core.EncryptionMeta, error) {
	if mp.encryptor == nil || !mu.Encrypted {
		return body, size, nil, nil
	}
	out, ciphertextSize, enc, err := mp.encryptWithUploadDEK(ctx, mu, body, size)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("encrypt part: %w", err)
	}
	return out, ciphertextSize, enc, nil
}

// multipartPartKey returns the temporary object key for a multipart part.
func multipartPartKey(uploadID string, partNumber int) string {
	return "__multipart/" + uploadID + "/" + strconv.Itoa(partNumber)
}

// fetchScopedUpload looks up the multipart upload and verifies it belongs
// to the (bucket, key) the request URL implies. Returns the same 404
// NoSuchUpload error for both missing and out-of-scope rows so callers
// cannot distinguish the two and probe for upload IDs across buckets.
// span must be the operation's pre-existing span (created at the entry
// point). Errors are recorded against it so the operation span shows
// the failure rather than a detached child span.
func (mp *Manager) fetchScopedUpload(ctx context.Context, span trace.Span, bucket, key, uploadID, operation string) (*core.MultipartUpload, error) {
	mu, err := mp.stores.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return nil, mp.core.ClassifyWriteError(span, operation, err)
	}
	if err := validateMultipartScope(mu, bucket, key); err != nil {
		observe.RecordSpanError(span, err)
		return nil, err
	}
	return mu, nil
}

// validateMultipartScope returns ErrMultipartUploadNotFound when the
// multipart upload's stored ObjectKey does not match the (bucket, key) the
// caller's request URL implies. The error code is the same one returned for
// a genuinely missing upload so a caller cannot probe for upload IDs across
// bucket boundaries by observing differing failure modes.
func validateMultipartScope(mu *core.MultipartUpload, bucket, key string) error {
	if mu == nil {
		return core.ErrMultipartUploadNotFound
	}
	if mu.ObjectKey != internalkey.Make(bucket, key) {
		return core.ErrMultipartUploadNotFound
	}
	return nil
}

// collectRequestedParts loads every part for uploadID, validates that all
// requested part numbers were uploaded, then returns the requested
// subset sorted in part-number order ready for assembly.
func (mp *Manager) collectRequestedParts(ctx context.Context, span trace.Span, uploadID string, partNumbers []int) ([]core.MultipartPart, error) {
	allParts, err := mp.stores.GetParts(ctx, uploadID)
	if err != nil {
		observe.RecordSpanError(span, err)
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
		observe.MarkSpanError(span, msg)
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

// formatPartNumbers formats a slice of part numbers for error messages.
func formatPartNumbers(parts []int) string {
	s := make([]string, len(parts))
	for i, pn := range parts {
		s[i] = strconv.Itoa(pn)
	}
	return strings.Join(s, ", ")
}

// AbortMultipartUpload cleans up an in-progress multipart upload, removing
// all part objects from the backend and the upload records from the database.
// The bucket/key arguments scope the operation to the requesting client's
// URL, matching them against the stored ObjectKey via validateMultipartScope
// so a caller for one bucket cannot abort an upload that belongs to another.
func (mp *Manager) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	const operation = "AbortMultipartUpload"
	ctx, span := telemetry.StartSpan(ctx, spanPrefix+operation,
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
func (mp *Manager) abortByMultipartRow(ctx context.Context, mu *core.MultipartUpload) error {
	ctx, span := telemetry.StartSpan(ctx, spanPrefix+"AbortMultipartUpload",
		telemetry.AttrUploadID.String(mu.UploadID),
	)
	defer span.End()
	const operation = "AbortMultipartUpload"
	start := time.Now()
	uploadID := mu.UploadID

	be, err := mp.core.GetBackend(mu.BackendName)
	if err != nil {
		observe.RecordSpanError(span, err)
		return err
	}

	parts, err := mp.stores.GetParts(ctx, uploadID)
	if err != nil {
		observe.RecordSpanError(span, err)
		return fmt.Errorf("failed to get parts for abort: %w", err)
	}

	for _, part := range parts {
		partKey := multipartPartKey(uploadID, part.PartNumber)
		mp.coord.DeleteOrEnqueue(ctx, be, mu.BackendName, partKey, "abort_part_cleanup", part.SizeBytes)
	}

	if err := mp.stores.DeleteMultipartUpload(ctx, uploadID); err != nil {
		observe.RecordSpanError(span, err)
		return err
	}

	mp.forgetUploadDEK(uploadID)

	// 1 abort API call. The N part DELETEs go through DeleteOrEnqueue,
	// which records them itself.
	mp.core.Acct().Operation(operation, mu.BackendName, start, nil)
	mp.core.Acct().APICall(mu.BackendName)

	pobserve.MultipartAborted(ctx, span, uploadID, mu.ObjectKey, mu.BackendName, len(parts))
	return nil
}

// CleanupStaleMultipartUploads aborts multipart uploads older than the given
// duration. Run periodically to prevent quota leaks from abandoned uploads.
func (mp *Manager) CleanupStaleMultipartUploads(ctx context.Context, olderThan time.Duration) {
	uploads, err := mp.stores.GetStaleMultipartUploads(ctx, olderThan)
	if err != nil {
		mp.log.ErrorContext(ctx, "failed to get stale multipart uploads", "error", err)
		return
	}

	cleaned := 0
	for i := range uploads {
		mu := &uploads[i]
		mp.log.InfoContext(ctx, "cleaning up stale multipart upload", "upload_id", mu.UploadID, "key", mu.ObjectKey)
		if err := mp.abortByMultipartRow(ctx, mu); err != nil {
			mp.log.ErrorContext(ctx, "failed to clean up upload", "upload_id", mu.UploadID, "error", err)
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
func (mp *Manager) AbortMultipartUploadsOnBackend(ctx context.Context, backendName string) {
	uploads, err := mp.stores.GetMultipartUploadsByBackend(ctx, backendName)
	if err != nil {
		mp.log.ErrorContext(ctx, "failed to list multipart uploads", "backend", backendName, "error", err)
		return
	}

	for i := range uploads {
		mu := &uploads[i]
		mp.log.InfoContext(ctx, "aborting multipart upload", "upload_id", mu.UploadID, "key", mu.ObjectKey)
		if err := mp.abortByMultipartRow(ctx, mu); err != nil {
			mp.log.ErrorContext(ctx, "failed to abort multipart upload",
				"upload_id", mu.UploadID, "error", err)
		}
	}
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
func (mp *Manager) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int) (string, error) {
	const operation = "CompleteMultipartUpload"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, spanPrefix+operation,
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
		observe.MarkSpanError(span, "another CompleteMultipartUpload in flight")
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
func (mp *Manager) completeMultipartUploadLocked(
	ctx context.Context,
	span trace.Span,
	operation, uploadID string,
	partNumbers []int,
	start time.Time,
) (string, error) {
	mu, err := mp.stores.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return "", mp.core.ClassifyWriteError(span, operation, err)
	}
	be, err := mp.core.GetBackend(mu.BackendName)
	if err != nil {
		observe.RecordSpanError(span, err)
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

	// Tee the plaintext stream through SHA-256 when integrity is on so
	// the assembled object lands with a content_hash matching the
	// regular PutObject path. Without this, the scrubber cannot verify
	// multipart-completed objects.
	hasher := mp.newIntegrityHasher()
	assembleReader := io.Reader(pr)
	if hasher != nil {
		assembleReader = io.TeeReader(pr, hasher)
	}

	uploadBody, uploadSize, enc, err := mp.buildAssembledUpload(ctx, span, mu, assembleReader, totalPlaintextSize)
	if err != nil {
		return "", err
	}

	// Final assembly PUT runs under the configured backend timeout.
	// The pipe reader is fed by the part-download goroutines,
	// so the timeout covers the full "stream parts -> assemble -> PUT"
	// pipeline; a tighter caller deadline (from the inbound HTTP
	// request) still wins.
	wctx, wcancel := mp.core.WithTimeout(ctx)
	defer wcancel()
	etag, err := be.PutObject(wctx, mu.ObjectKey, uploadBody, uploadSize, mu.ContentType, mu.Metadata)
	if err != nil {
		pipeCancel()
		pr.Close()
		observe.RecordSpanError(span, err)
		return "", fmt.Errorf("failed to upload final object: %w", err)
	}
	pr.Close()

	enc = stampContentHash(enc, hasher)

	if err := mp.coord.RecordObjectOrCleanup(ctx, span, be, mu.ObjectKey, mu.BackendName, uploadSize, enc); err != nil {
		return "", err
	}

	// N part GETs + 1 assembled PUT (Ingress charges that one). The N
	// cleanup DELETEs of the part temp keys go through DeleteOrEnqueue,
	// which records them itself.
	mp.core.Acct().Operation(operation, mu.BackendName, start, nil)
	mp.core.Acct().APICalls(mu.BackendName, int64(len(parts)))
	mp.core.Acct().Ingress(mu.BackendName, uploadSize)

	pobserve.MultipartCompleted(ctx, span, mu.ObjectKey, mu.BackendName, uploadID, totalPlaintextSize, len(parts))
	mp.invalidateCache(mu.ObjectKey)
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
func (mp *Manager) cleanupCompletedUpload(ctx context.Context, span trace.Span, be s3be.ObjectBackend, mu *core.MultipartUpload, uploadID string, parts []core.MultipartPart) {
	for _, part := range parts {
		partKey := multipartPartKey(uploadID, part.PartNumber)
		mp.coord.DeleteOrEnqueue(ctx, be, mu.BackendName, partKey, "complete_part_cleanup", part.SizeBytes)
	}
	if err := mp.stores.DeleteMultipartUpload(ctx, uploadID); err != nil {
		span.RecordError(err)
	}
	mp.forgetUploadDEK(uploadID)
}

// newIntegrityHasher returns a fresh SHA-256 hasher when integrity
// verification is enabled, or nil to signal "skip hashing." Mirrors the
// gate used by the regular PutObject path so the multipart-completed
// object carries the same content_hash semantics.
func (mp *Manager) newIntegrityHasher() hash.Hash {
	if mp.integrityCfg == nil {
		return nil
	}
	icfg := mp.integrityCfg.Load()
	if icfg == nil || !icfg.Enabled {
		return nil
	}
	return sha256.New()
}

// stampContentHash finalises the hasher (when one was used) and writes
// the resulting hex digest onto enc. When integrity is disabled hasher
// is nil and the original enc is returned unchanged; when enc is nil
// and a hash was computed, a fresh EncryptionMeta is allocated so the
// store layer receives the hash.
func stampContentHash(enc *core.EncryptionMeta, hasher hash.Hash) *core.EncryptionMeta {
	if hasher == nil {
		return enc
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if enc == nil {
		return &core.EncryptionMeta{ContentHash: digest}
	}
	enc.ContentHash = digest
	return enc
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
func (mp *Manager) buildAssembledUpload(
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
		observe.RecordSpanError(span, err)
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
func (mp *Manager) streamPartsThroughPipe(
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
func (mp *Manager) streamOnePart(
	ctx context.Context,
	be s3be.ObjectBackend,
	bw io.Writer,
	uploadID string,
	part *core.MultipartPart,
) error {
	partKey := multipartPartKey(uploadID, part.PartNumber)
	bctx, bcancel := mp.core.WithTimeout(ctx)
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

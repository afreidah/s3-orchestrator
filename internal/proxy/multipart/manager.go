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
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/etag"
	pobserve "github.com/afreidah/s3-orchestrator/internal/proxy/observe"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
	"github.com/afreidah/s3-orchestrator/internal/util/materialize"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// spanPrefix is prepended to every OpenTelemetry span name this package
// creates so traces distinguish the manager layer ("Manager UploadPart")
// from the backend layer ("Backend UploadPart") in the same trace.
const spanPrefix = "Manager "

// abortScanPageSize is how many uploads one page of an abort scan holds.
const abortScanPageSize = 100

// Stores is the persistence surface multipart needs: multipart row and part
// operations, plus the advisory lock that serializes stale-upload sweeps.
type Stores interface {
	core.MultipartStore
	core.AdvisoryLocker
}

// Manager handles the multipart upload lifecycle.
//
// dekCache holds each upload's unwrapped DEK so repeated UploadPart calls pay
// for one KeyProvider unwrap. Its TTL matches the stale-upload sweep interval,
// so an abandoned upload's DEK does not outlive the upload. Concurrent calls
// for the same upload on a cold cache each issue their own unwrap.
type Manager struct {
	core               Runtime                // infrastructure subset: backends, usage, timeout, error classification, metrics
	coord              *writepath.Coordinator // write-path helpers shared with the object manager
	stores             Stores                 // multipart row/part operations and WithAdvisoryLock
	encryptor          *encryption.Encryptor
	codec              Codec
	compression        config.CompressionConfig
	objectCache        objcache.ObjectCache
	dekCache           *syncutil.TTLCache[string, []byte]
	integrityCfg       *syncutil.AtomicConfig[config.IntegrityConfig] // nil-safe; controls plaintext SHA-256 on Complete
	enforceMinPartSize bool                                           // reject non-final parts below the S3 5 MiB floor
	log                *slog.Logger
}

// New creates a Manager. Core, Coord and Stores must be non-nil. When
// IntegrityCfg is nil or disabled, CompleteMultipartUpload skips the plaintext
// hash that populates content_hash on the recorded location.
func New(deps *Deps) *Manager {
	must.NotNil("deps", deps)
	must.NotNil("Core", deps.Core)
	must.NotNil("Coord", deps.Coord)
	must.NotNil("Stores", deps.Stores)
	return &Manager{
		core:               deps.Core,
		coord:              deps.Coord,
		stores:             deps.Stores,
		encryptor:          deps.Encryptor,
		codec:              deps.Codec,
		compression:        deps.Compression,
		objectCache:        deps.ObjectCache,
		dekCache:           syncutil.NewTTLCache[string, []byte](deps.DEKCacheTTL),
		integrityCfg:       deps.IntegrityCfg,
		enforceMinPartSize: deps.EnforceMinPartSize,
		log:                slog.Default().With(logfmt.Component("multipart")),
	}
}

// Deps groups the multipart manager's constructor parameters: backend
// runtime, shared write coordinator, store surface, optional encryption /
// compression / object cache, the DEK-cache TTL, and the shared integrity
// config. Codec is supplied whether or not Compression.Enabled, matching the
// object manager: an assembled object is encoded only when both are set.
type Deps struct {
	Core         Runtime
	Coord        *writepath.Coordinator
	Stores       Stores
	Encryptor    *encryption.Encryptor // nil when encryption is disabled
	Codec        Codec                 // nil when no codec is configured
	Compression  config.CompressionConfig
	ObjectCache  objcache.ObjectCache // nil when object caching is disabled
	DEKCacheTTL  time.Duration
	IntegrityCfg *syncutil.AtomicConfig[config.IntegrityConfig]

	EnforceMinPartSize bool // every non-final part must meet the S3 5 MiB floor
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

// CreateUploadRequest is one CreateMultipartUpload call's inputs.
//
// Tags are validated by the transport before the upload is opened, so an
// unusable set costs no upload slot and no parts. They are held on the upload
// row and applied to the object CompleteMultipartUpload produces.
type CreateUploadRequest struct {
	Key         string
	ContentType string
	Metadata    map[string]string
	Tags        []core.Tag
}

// CreateMultipartUpload initiates a multipart upload by selecting a backend
// with available quota and recording the upload in the database. With
// encryption configured it wraps one DEK here and persists it on the upload
// row; every UploadPart and the assembled object share that DEK.
func (mp *Manager) CreateMultipartUpload(ctx context.Context, req *CreateUploadRequest) (string, string, error) {
	const operation = s3op.CreateMultipartUpload
	key := req.Key
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, spanPrefix+operation.String(),
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()

	// The packed format mirrors object_locations.encryption_key, but the base
	// nonce is zero: the upload row never produces ciphertext, and each part
	// and the assembled object store their own base nonce.
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

	// The final size is unknown, so nothing is claimed: each part is counted
	// against the backend by its own row as it lands.
	uploadID := audit.NewID()
	backendName, err := mp.coord.ClaimUploadTarget(ctx, span, operation, &core.CreateMultipartUploadParams{
		UploadID:      uploadID,
		ObjectKey:     key,
		ContentType:   req.ContentType,
		Metadata:      req.Metadata,
		EncryptionKey: encryptionKey,
		KeyID:         keyID,
		Tags:          req.Tags,
	})
	if err != nil {
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
	const operation = s3op.UploadPart
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, spanPrefix+operation.String(),
		telemetry.AttrUploadID.String(uploadID),
		telemetry.AttrPartNumber.Int(partNumber),
	)
	defer span.End()

	if partNumber < 1 || partNumber > 10000 {
		err := &core.S3Error{StatusCode: http.StatusBadRequest, Code: "InvalidArgument", Message: "Part number must be between 1 and 10000"}
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

	// Checked against what the part will occupy, which for an encrypted upload
	// is the envelope rather than the bytes the client sent.
	if !mp.core.Usage().WithinLimits(mu.BackendName, []s3op.Operation{s3op.UploadPart}, 0, mp.physicalPartSize(mu, size)) {
		observe.MarkSpanError(span, "usage limits exceeded")
		return "", core.ErrInsufficientStorage
	}

	// The part's own MD5 is taken off the client's bytes on their way to the
	// backend, before any encryption layer: the object's ETag is the MD5 of
	// the concatenated part digests, and a digest of the stored envelope would
	// not be one S3 could have produced.
	partDigest := etag.NewHasher()
	uploadBody, uploadSize, form, err := mp.prepareUploadPartBody(ctx, mu, io.TeeReader(body, partDigest), size)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}

	// Store part under a temp key
	partKey := multipartPartKey(uploadID, partNumber)
	bctx, bcancel := mp.core.WithTimeout(ctx)
	defer bcancel()
	storedETag, err := be.PutObject(bctx, partKey, uploadBody, uploadSize, "application/octet-stream", nil)
	if err != nil {
		mp.core.Acct().APICall(s3op.UploadPart, mu.BackendName) // API call was made even on failure
		observe.RecordSpanError(span, err)
		return "", fmt.Errorf("failed to upload part: %w", err)
	}

	plaintextETag := etag.Hex(partDigest)
	if err := mp.stores.RecordPart(ctx, &core.RecordPartParams{
		UploadID: uploadID, PartNumber: partNumber, ETag: storedETag,
		PlaintextETag: plaintextETag, SizeBytes: uploadSize, Form: form,
	}); err != nil {
		mp.log.ErrorContext(ctx, "recordPart failed, cleaning up part object",
			"upload_id", uploadID, "part", partNumber, "error", err)
		mp.coord.RecoverFromRecordFailure(ctx, be, partCleanup(mu.BackendName, partKey, "orphan_part_record_failed", uploadSize))
		observe.RecordSpanError(span, err)
		return "", fmt.Errorf("failed to record part: %w", err)
	}

	// Charged at what was sent, not at what the client handed over: an
	// encrypted part carries its own envelope, and a large upload repeats that
	// difference once per part.
	mp.core.Acct().PutSuccess(operation, mu.BackendName, uploadSize, start)
	pobserve.UploadPartCompleted(ctx, span, mu.ObjectKey, mu.BackendName, uploadID, partNumber, size)
	// The client is given the MD5 of its own bytes, which is what S3 returns
	// and what it will send back in the completion manifest.
	return etag.Single(plaintextETag), nil
}

// physicalPartSize reports how many bytes a part of this size will occupy once
// encrypted. Parts are never compressed, so this is known before the read.
func (mp *Manager) physicalPartSize(mu *core.MultipartUpload, size int64) int64 {
	if mp.encryptor == nil || !mu.Encrypted {
		return size
	}
	return mp.encryptor.CiphertextSize(size)
}

// -------------------------------------------------------------------------
// READ-ONLY ACCESSORS
// -------------------------------------------------------------------------

// ListMultipartUploads returns active multipart uploads matching the given
// prefix, up to maxUploads results. Pass-through to the metadata store.
func (mp *Manager) ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]core.MultipartUpload, error) {
	return mp.stores.ListMultipartUploads(ctx, prefix, maxUploads)
}

// GetParts returns all parts for a multipart upload, each reporting the ETag
// UploadPart handed the client rather than the one the backend holds. A resumed
// upload rebuilds its completion manifest from this list, so it must get back
// the values completion validates against; the two differ once the stored part
// is an encryption envelope.
func (mp *Manager) GetParts(ctx context.Context, bucket, key, uploadID string) ([]core.MultipartPart, error) {
	const operation = s3op.GetParts
	ctx, span := telemetry.StartSpan(ctx, spanPrefix+operation.String(),
		telemetry.AttrUploadID.String(uploadID),
	)
	defer span.End()
	if _, err := mp.fetchScopedUpload(ctx, span, bucket, key, uploadID, operation); err != nil {
		return nil, err
	}
	parts, err := mp.stores.GetParts(ctx, uploadID)
	if err != nil {
		return nil, err
	}
	for i := range parts {
		presentToClient(&parts[i])
	}
	return parts, nil
}

// ListParts returns one page of an upload's parts: up to maxParts numbered
// above partNumberMarker, and whether more follow. ETags are the ones UploadPart
// handed the client, for the reason GetParts gives.
func (mp *Manager) ListParts(ctx context.Context, bucket, key, uploadID string, partNumberMarker, maxParts int) ([]core.MultipartPart, bool, error) {
	const operation = s3op.GetParts
	ctx, span := telemetry.StartSpan(ctx, spanPrefix+operation.String(),
		telemetry.AttrUploadID.String(uploadID),
	)
	defer span.End()
	if _, err := mp.fetchScopedUpload(ctx, span, bucket, key, uploadID, operation); err != nil {
		return nil, false, err
	}
	if maxParts == 0 {
		// A zero-size page still reports whether any parts lie past the marker.
		rest, err := mp.stores.ListParts(ctx, uploadID, partNumberMarker, 1)
		if err != nil {
			return nil, false, err
		}
		return nil, len(rest) > 0, nil
	}
	// One extra row says whether the page is the last without a count query.
	parts, err := mp.stores.ListParts(ctx, uploadID, partNumberMarker, maxParts+1)
	if err != nil {
		return nil, false, err
	}
	truncated := len(parts) > maxParts
	if truncated {
		parts = parts[:maxParts]
	}
	for i := range parts {
		presentToClient(&parts[i])
	}
	return parts, truncated, nil
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

// encryptWithUploadDEK encrypts body under the upload-level DEK from mu and
// returns the ciphertext reader, its size, and the StoredForm to persist on the
// resulting part or object row. The caller decides whether to encrypt at all.
func (mp *Manager) encryptWithUploadDEK(ctx context.Context, mu *core.MultipartUpload, body io.Reader, size int64) (io.Reader, int64, *core.StoredForm, error) {
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
	return result.Body, result.CiphertextSize, &core.StoredForm{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(result.BaseNonce, result.WrappedDEK),
		KeyID:         result.KeyID,
		PlaintextSize: size,
	}, nil
}

// prepareUploadPartBody returns the body, size and stored form for one part,
// encrypted under the upload's cached data key when the upload is encrypted.
func (mp *Manager) prepareUploadPartBody(ctx context.Context, mu *core.MultipartUpload, body io.Reader, size int64) (io.Reader, int64, *core.StoredForm, error) {
	if mp.encryptor == nil || !mu.Encrypted {
		return body, size, nil, nil
	}
	out, ciphertextSize, form, err := mp.encryptWithUploadDEK(ctx, mu, body, size)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("encrypt part: %w", err)
	}
	return out, ciphertextSize, form, nil
}

// partCleanup describes one part object for the cleanup paths. A part is
// already stored under a path unique to its upload and number, so it is its own
// storage key and needs no per-write suffix; the object key is the same string
// because a part is not an object any client can name.
func partCleanup(backendName, partKey, reason string, size int64) *core.CleanupRequest {
	return &core.CleanupRequest{
		BackendName: backendName,
		ObjectKey:   partKey,
		StorageKey:  partKey,
		Reason:      reason,
		SizeBytes:   size,
	}
}

// deleteParts removes an upload's part objects in as few backend requests as
// the backend allows, queueing any it cannot remove for retry.
func (mp *Manager) deleteParts(ctx context.Context, mu *core.MultipartUpload, parts []core.MultipartPart, reason string) {
	reqs := make([]*core.CleanupRequest, len(parts))
	for i := range parts {
		reqs[i] = partCleanup(mu.BackendName, multipartPartKey(mu.UploadID, parts[i].PartNumber), reason, parts[i].SizeBytes)
	}
	mp.coord.DeleteAllOrEnqueue(ctx, reqs)
}

// multipartPartKey returns the temporary object key for a multipart part.
func multipartPartKey(uploadID string, partNumber int) string {
	return "__multipart/" + uploadID + "/" + strconv.Itoa(partNumber)
}

// fetchScopedUpload looks up the multipart upload and verifies it belongs to
// the (bucket, key) the request URL implies. Missing and out-of-scope rows
// return the same NoSuchUpload error, so callers cannot probe for upload IDs
// across buckets. Errors are recorded on span, the operation's own span.
func (mp *Manager) fetchScopedUpload(ctx context.Context, span trace.Span, bucket, key, uploadID string, operation s3op.Operation) (*core.MultipartUpload, error) {
	mu, err := mp.stores.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return nil, mp.core.ClassifyWriteError(span, operation.String(), err)
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
	for i := range allParts {
		uploaded[allParts[i].PartNumber] = true
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
		return nil, &core.S3Error{StatusCode: http.StatusBadRequest, Code: "InvalidPart", Message: msg}
	}

	requested := make(map[int]bool, len(partNumbers))
	for _, pn := range partNumbers {
		requested[pn] = true
	}
	var parts []core.MultipartPart
	for i := range allParts {
		if requested[allParts[i].PartNumber] {
			parts = append(parts, allParts[i])
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
	const operation = s3op.AbortMultipartUpload
	ctx, span := telemetry.StartSpan(ctx, spanPrefix+operation.String(),
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
	const operation = s3op.AbortMultipartUpload
	start := time.Now()
	uploadID := mu.UploadID

	if _, err := mp.core.GetBackend(mu.BackendName); err != nil {
		observe.RecordSpanError(span, err)
		return err
	}

	parts, err := mp.stores.GetParts(ctx, uploadID)
	if err != nil {
		observe.RecordSpanError(span, err)
		return fmt.Errorf("failed to get parts for abort: %w", err)
	}

	mp.deleteParts(ctx, mu, parts, "abort_part_cleanup")

	if err := mp.stores.DeleteMultipartUpload(ctx, uploadID); err != nil {
		observe.RecordSpanError(span, err)
		return err
	}

	mp.forgetUploadDEK(uploadID)

	// 1 abort API call. The part deletes are charged where they are sent.
	mp.core.Acct().Operation(operation, mu.BackendName, start, nil)
	mp.core.Acct().APICall(operation, mu.BackendName)

	pobserve.MultipartAborted(ctx, span, uploadID, mu.ObjectKey, mu.BackendName, len(parts))
	return nil
}

// CleanupStaleMultipartUploads aborts multipart uploads older than the given
// duration. Run periodically to prevent quota leaks from abandoned uploads.
func (mp *Manager) CleanupStaleMultipartUploads(ctx context.Context, olderThan time.Duration) {
	sum := mp.abortMatching(ctx, core.MultipartUploadFilter{CreatedBefore: time.Now().Add(-olderThan)})
	if sum.Succeeded > 0 {
		audit.Log(ctx, "storage.MultipartCleanup",
			slog.Int("cleaned", sum.Succeeded),
			slog.Int("total_stale", sum.Planned),
		)
	}
}

// AbortMultipartUploadsOnBackend aborts all in-progress multipart uploads
// on the given backend.
func (mp *Manager) AbortMultipartUploadsOnBackend(ctx context.Context, backendName string) {
	mp.abortMatching(ctx, core.MultipartUploadFilter{Backend: backendName})
}

// abortMatching aborts every upload filter selects, walking them a page at a
// time by upload id, and reports the tally. An upload that fails to abort is
// logged and passed over, so the next run retries it.
func (mp *Manager) abortMatching(ctx context.Context, filter core.MultipartUploadFilter) batch.Summary {
	pager := batch.Pager[core.MultipartUpload, string]{
		PageSize: batch.FixedPage(abortScanPageSize),
		List: func(ctx context.Context, limit int, after string) ([]core.MultipartUpload, error) {
			return mp.stores.ScanMultipartUploads(ctx, filter, limit, after)
		},
		CursorOf: func(mu core.MultipartUpload) string { return mu.UploadID },
	}
	runner := batch.Runner[core.MultipartUpload]{Name: "multipart-abort", Concurrency: 1}
	var total batch.Summary
	stop, err := pager.Walk(ctx, func(ctx context.Context, uploads []core.MultipartUpload) (batch.Step, error) {
		sum := runner.Run(ctx, uploads, func(ctx context.Context, mu core.MultipartUpload) batch.ItemResult {
			mp.log.InfoContext(ctx, "aborting multipart upload", "upload_id", mu.UploadID, "key", mu.ObjectKey, "backend", mu.BackendName)
			if err := mp.abortByMultipartRow(ctx, &mu); err != nil {
				mp.log.ErrorContext(ctx, "failed to abort multipart upload", "upload_id", mu.UploadID, "error", err)
				return batch.ItemResult{Outcome: batch.ItemFailed}
			}
			return batch.ItemResult{Outcome: batch.ItemSucceeded}
		})
		total = total.Plus(sum)
		return batch.Step{Progress: sum.Succeeded}, nil
	})
	if stop == batch.Errored {
		mp.log.ErrorContext(ctx, "failed to list multipart uploads",
			"backend", filter.Backend, "created_before", filter.CreatedBefore, "error", err)
	}
	return total
}

// CompleteMultipartUpload streams the parts into one assembled object, records
// its location, and removes the parts. It runs under an advisory lock keyed by
// uploadID so two concurrent completions cannot both assemble the object; the
// second caller fails fast with 409 OperationAborted.
func (mp *Manager) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, manifest []core.CompletePart) (string, error) {
	const operation = s3op.CompleteMultipartUpload
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, spanPrefix+operation.String(),
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
		etag, inner = mp.completeMultipartUploadLocked(ctx, span, operation, uploadID, manifest, start)
		return inner
	})
	if err != nil {
		return "", err
	}
	if !acquired {
		observe.MarkSpanError(span, "another CompleteMultipartUpload in flight")
		return "", &core.S3Error{
			StatusCode: http.StatusConflict,
			Code:       "OperationAborted",
			Message:    "Another CompleteMultipartUpload is already in progress for this upload",
		}
	}
	return etag, nil
}

// completeMultipartUploadLocked runs the assembly under CompleteMultipartUpload's
// advisory lock. The order is validate, assemble, commit, then drop the parts:
// any failure before the commit leaves the parts and the upload row intact so
// the client can retry, and the stale-multipart sweep reaps an upload that is
// abandoned after a failure.
func (mp *Manager) completeMultipartUploadLocked(
	ctx context.Context,
	span trace.Span,
	operation s3op.Operation,
	uploadID string,
	manifest []core.CompletePart,
	start time.Time,
) (string, error) {
	mu, err := mp.stores.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return "", mp.core.ClassifyWriteError(span, operation.String(), err)
	}
	be, err := mp.core.GetBackend(mu.BackendName)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}

	// Shape first: a malformed manifest is rejected without reading parts.
	if err := validateManifestShape(manifest); err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}

	partNumbers := make([]int, len(manifest))
	for i, p := range manifest {
		partNumbers[i] = p.PartNumber
	}
	parts, err := mp.collectRequestedParts(ctx, span, uploadID, partNumbers)
	if err != nil {
		return "", err
	}

	// ETag and size checks run against the same rows assembly will read,
	// under the same lock, so a part replaced between validation and
	// assembly cannot slip through.
	if err := validateManifestAgainstStored(manifest, parts, mp.enforceMinPartSize); err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}

	totalPlaintextSize := sumPlaintextSize(parts)

	// The identity is known before the bytes move: the composite ETag is built
	// from the part digests already recorded, so an intent the reaper promotes
	// carries the same answer the client was given.
	identity, err := assembledIdentity(mu, parts)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}

	pr, pipeCancel := mp.streamPartsThroughPipe(ctx, be, uploadID, parts)
	defer pipeCancel()

	// Tee the plaintext stream through SHA-256 when integrity is on so
	// the assembled object lands with a content_hash matching the
	// regular PutObject path. Without this, the scrubber cannot verify
	// multipart-completed objects.
	hasher := mp.newIntegrityHasher()
	// An upload whose parts predate per-part digests has no composite, and the
	// MD5 of the assembled bytes is the only ETag left to give it. Every other
	// upload already knows its own and must not pay for a second pass over
	// every byte it assembles.
	var assemblyDigest hash.Hash
	if identity.ETag == "" {
		assemblyDigest = etag.NewHasher()
	}
	assembleReader := teeThrough(pr, assemblyDigest, hasher)

	// Compression runs between the part pipe and the encryptor, the order the
	// single-object path uses and the only one that works: ciphertext does not
	// compress.
	stored, err := mp.compressAssembly(assembleReader, totalPlaintextSize)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}
	defer stored.cleanup()

	uploadBody, uploadSize, form, err := mp.buildAssembledUpload(ctx, span, mu, stored.body, stored.size)
	if err != nil {
		return "", err
	}
	form = stored.applyMeta(form, mp.compression.Level, totalPlaintextSize)

	// Record the intent before the assembly PUT so a crash between the PUT and
	// the commit leaves the pending reaper a record to finish or remove, rather
	// than bytes that reconcile would import as a real object.
	//
	// form has no content hash yet, since the digest is known only once the
	// stream drains; a reaper-promoted intent commits without one and the
	// scrubber backfills it.
	//
	// The claim is pinned to the upload's own backend because the parts are
	// already there: a backend without room must fail, not place it elsewhere.
	intent := writepath.NewPendingIntent(mu.ObjectKey, uploadSize, form, identity)
	if _, err := mp.coord.ClaimWriteTarget(ctx, intent, []string{mu.BackendName}); err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}
	intentID := intent.IntentID

	// The pipe is fed by the part downloads, so this timeout covers the whole
	// stream-parts, assemble and PUT pipeline.
	wctx, wcancel := mp.core.WithTimeout(ctx)
	defer wcancel()
	// Assembled under the intent's own storage key, so a double completion or a
	// concurrent write to the key leaves two distinct objects instead of two
	// writers racing at one path. The backend's ETag describes the stored bytes
	// and is discarded in favor of the composite.
	_, err = be.PutObject(wctx, intent.StorageKey, uploadBody, uploadSize, mu.ContentType, mu.Metadata)
	if err != nil {
		pipeCancel()
		pr.Close()
		observe.RecordSpanError(span, err)
		// Parts and the upload row stay put so the client can retry. The
		// intent stays too: a PUT error does not prove the bytes are absent,
		// so the reaper HEADs the backend and resolves it either way.
		return "", fmt.Errorf("failed to upload final object: %w", err)
	}
	pr.Close()

	form = stampContentHash(form, hasher)
	if identity.ETag == "" {
		identity.ETag = etag.Single(etag.Hex(assemblyDigest))
	}

	// The create call's tags land in the same transaction, so a completed upload
	// is never briefly untagged. The bytes are charged here from what the ledger
	// recorded, with no reservation.
	if err := mp.coord.RecordObjectAndPromoteIntent(ctx, span, &core.RecordObjectRequest{
		Key: mu.ObjectKey, Size: uploadSize, Form: form, Identity: identity, Tags: mu.Tags,
		Copies: []core.ObjectCopy{{Backend: mu.BackendName, IntentID: intentID, StorageKey: intent.StorageKey}},
	}); err != nil {
		return "", err
	}

	// Only now is the object durably committed, so the source parts are safe
	// to drop. Every failure path above returns with the parts and the upload
	// row intact, leaving the completion retryable.
	mp.cleanupCompletedUpload(ctx, span, mu, uploadID, parts)

	// Assembly spends egress equal to the parts and ingress equal to the result.
	// Each Egress records its own API call for a part GET; the part deletes are
	// charged where they are sent.
	mp.core.Acct().Operation(operation, mu.BackendName, start, nil)
	for i := range parts {
		mp.core.Acct().Egress(s3op.GetObject, mu.BackendName, parts[i].SizeBytes)
	}
	mp.core.Acct().Ingress(s3op.PutObject, mu.BackendName, uploadSize)

	pobserve.MultipartCompleted(ctx, span, mu.ObjectKey, mu.BackendName, uploadID, totalPlaintextSize, len(parts))
	mp.invalidateCache(mu.ObjectKey)
	return identity.ETag, nil
}

// teeThrough returns r with every non-nil hasher fed from it, so one drain of
// the assembly stream produces every digest the completion needs.
func teeThrough(r io.Reader, hashers ...hash.Hash) io.Reader {
	out := r
	for _, h := range hashers {
		if h != nil {
			out = io.TeeReader(out, h)
		}
	}
	return out
}

// assembledIdentity builds the identity a client is told for the completed
// object: the composite ETag over the part digests, plus the content type and
// metadata the create call carried. The ETag is empty when any part lacks a
// digest; the caller then fills it with the MD5 of the assembled bytes.
func assembledIdentity(mu *core.MultipartUpload, parts []core.MultipartPart) (*core.ObjectIdentity, error) {
	digests := make([]string, len(parts))
	for i := range parts {
		digests[i] = parts[i].PlaintextETag
	}
	composite, err := etag.Multipart(digests)
	if err != nil {
		return nil, err
	}
	meta := mu.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	return &core.ObjectIdentity{
		ETag:         composite,
		ContentType:  mu.ContentType,
		UserMetadata: meta,
	}, nil
}

// cleanupCompletedUpload removes the part objects, the multipart_uploads row and
// the cached DEK of a durably committed upload. Call it only after the commit:
// on a failure path it would destroy the parts a retry needs. Each step is best
// effort and does not stop the rest.
func (mp *Manager) cleanupCompletedUpload(ctx context.Context, span trace.Span, mu *core.MultipartUpload, uploadID string, parts []core.MultipartPart) {
	mp.deleteParts(ctx, mu, parts, "complete_part_cleanup")
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
// the resulting hex digest onto form. When integrity is disabled hasher
// is nil and the original form is returned unchanged; when form is nil
// and a hash was computed, a fresh StoredForm is allocated so the
// store layer receives the hash.
func stampContentHash(form *core.StoredForm, hasher hash.Hash) *core.StoredForm {
	if hasher == nil {
		return form
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if form == nil {
		return &core.StoredForm{ContentHash: digest}
	}
	form.ContentHash = digest
	return form
}

// sumPlaintextSize returns the total plaintext byte count across parts.
// Encrypted parts contribute PlaintextSize; unencrypted parts contribute
// SizeBytes.
func sumPlaintextSize(parts []core.MultipartPart) int64 {
	var total int64
	for i := range parts {
		if parts[i].Encrypted {
			total += parts[i].PlaintextSize
		} else {
			total += parts[i].SizeBytes
		}
	}
	return total
}

// assembledBody is the stream the assembly PUT sends, plus what has to be said
// about it: size is what will land on the backend, and compressed reports
// whether those bytes are an encoding of the object or the object itself.
//
// encoded and decoded are the resources behind that stream, held so cleanup can
// release exactly the ones a given path opened.
type assembledBody struct {
	body       io.Reader
	size       int64
	compressed bool
	encoded    *materialize.Body
	decoded    io.Closer
}

// cleanup releases whatever the assembly buffered. Safe on every path,
// including the one that buffered nothing.
func (a *assembledBody) cleanup() {
	if a.decoded != nil {
		_ = a.decoded.Close()
	}
	if a.encoded != nil {
		a.encoded.Cleanup()
	}
}

// applyMeta records how the assembled bytes were encoded, allocating a form when
// nothing upstream needed one. LogicalSize is the size the client uploaded
// across all parts, which is the only place that number survives once the row's
// SizeBytes counts the encoding instead.
func (a *assembledBody) applyMeta(form *core.StoredForm, level string, logicalSize int64) *core.StoredForm {
	if !a.compressed {
		return form
	}
	if form == nil {
		form = &core.StoredForm{}
	}
	form.CompressionAlgorithm = compression.Algorithm
	form.CompressionLevel = level
	form.CompressionFormatVersion = compression.FormatVersion
	form.LogicalSize = logicalSize
	return form
}

// compressOnComplete reports whether an assembled object of this size should be
// encoded, using the same gate as a single-object PUT.
func (mp *Manager) compressOnComplete(size int64) bool {
	return mp.codec != nil && mp.compression.Enabled && size >= mp.compression.MinSize
}

// compressAssembly encodes the assembled plaintext when compression applies and
// reports what the PUT should send. The encoding is buffered because a backend
// PUT declares its size up front. An encoding that misses min_ratio is decoded
// back out of that buffer, since the part pipe delivers the plaintext only once
// and re-reading the parts would charge their egress a second time.
func (mp *Manager) compressAssembly(src io.Reader, totalPlaintextSize int64) (*assembledBody, error) {
	if !mp.compressOnComplete(totalPlaintextSize) {
		if mp.codec != nil && mp.compression.Enabled {
			telemetry.CompressionSkippedTotal.WithLabelValues(telemetry.CompressionSkipMinSize).Inc()
		}
		return &assembledBody{body: src, size: totalPlaintextSize}, nil
	}

	buf, err := materialize.NewEmpty(totalPlaintextSize)
	if err != nil {
		return nil, fmt.Errorf("buffer assembled object: %w", err)
	}
	a := &assembledBody{encoded: buf}

	encodedSize, err := mp.codec.Compress(buf.Writer(), src)
	if err != nil {
		a.cleanup()
		telemetry.CompressionErrorsTotal.WithLabelValues(telemetry.CompressionOpEncode).Inc()
		return nil, fmt.Errorf("compress assembled object: %w", err)
	}
	reader, err := buf.Reader()
	if err != nil {
		a.cleanup()
		return nil, fmt.Errorf("read back assembled object: %w", err)
	}

	if compression.WorthStoring(totalPlaintextSize, encodedSize, mp.compression.MinRatio) {
		telemetry.RecordCompressed(totalPlaintextSize, encodedSize)
		a.body, a.size, a.compressed = reader, encodedSize, true
		return a, nil
	}
	telemetry.CompressionSkippedTotal.WithLabelValues(telemetry.CompressionSkipMinRatio).Inc()

	plain, err := mp.codec.Decompress(reader)
	if err != nil {
		a.cleanup()
		return nil, fmt.Errorf("decode discarded encoding: %w", err)
	}
	a.body, a.size, a.decoded = plain, totalPlaintextSize, plain
	return a, nil
}

// buildAssembledUpload returns the body the assembly PUT sends. With encryption
// configured it encrypts the plaintext pipe under the upload-level DEK from mu,
// so the assembled object shares its DEK with every part.
func (mp *Manager) buildAssembledUpload(
	ctx context.Context,
	span trace.Span,
	mu *core.MultipartUpload,
	pr io.Reader,
	totalPlaintextSize int64,
) (io.Reader, int64, *core.StoredForm, error) {
	if mp.encryptor == nil {
		return pr, totalPlaintextSize, nil, nil
	}
	out, ciphertextSize, form, err := mp.encryptWithUploadDEK(ctx, mu, pr, totalPlaintextSize)
	if err != nil {
		observe.RecordSpanError(span, err)
		return nil, 0, nil, fmt.Errorf("encrypt final object: %w", err)
	}
	return out, ciphertextSize, form, nil
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
		decrypted, _, decErr := mp.encryptor.DecryptStored(ctx, result.Body, part.EncryptionKey, part.KeyID, part.PlaintextSize, nil)
		if decErr != nil {
			if errors.Is(decErr, encryption.ErrInvalidKeyData) {
				return fmt.Errorf("unpack part %d key: %w", part.PartNumber, decErr)
			}
			return fmt.Errorf("decrypt part %d: %w", part.PartNumber, decErr)
		}
		src = decrypted
	}

	if _, err := bufpool.Copy(bw, src); err != nil {
		return fmt.Errorf("failed to stream part %d: %w", part.PartNumber, err)
	}
	return nil
}

// CountActiveMultipartUploads returns the number of in-progress uploads whose
// key falls under bucketPrefix. Lives here rather than on a facade because
// this is the type that already holds the multipart store.
func (mp *Manager) CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error) {
	return mp.stores.CountActiveMultipartUploads(ctx, bucketPrefix)
}

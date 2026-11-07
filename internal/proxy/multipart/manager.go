// -------------------------------------------------------------------------------
// Multipart Manager
//
// Author: Alex Freidah
//
// Manager owns the multipart-upload lifecycle: creation,
// per-part uploads, completion, abort/cleanup, the encryption helpers
// used by parts and the assembled object, the part/upload-row helpers
// shared across paths, and the completion mark CompleteMultipartUpload
// holds while it assembles.
// -------------------------------------------------------------------------------

package multipart

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/cycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/etag"
	pobserve "github.com/afreidah/s3-orchestrator/internal/proxy/observe"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
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
// operations, including the completion mark.
type Stores interface {
	core.MultipartStore
}

// Manager handles the multipart upload lifecycle.
//
// dekCache holds each upload's unwrapped DEK so repeated UploadPart calls pay
// for one KeyProvider unwrap. Its TTL matches the stale-upload sweep interval,
// so an abandoned upload's DEK does not outlive the upload. Concurrent calls
// for the same upload on a cold cache each issue their own unwrap.
type Manager struct {
	d        Deps
	dekCache *syncutil.TTLCache[string, []byte]
	log      *slog.Logger
}

// New creates a Manager. Core, Coord and Stores must be non-nil. When
// IntegrityCfg is nil or disabled, CompleteMultipartUpload skips the plaintext
// hash that populates content_hash on the recorded location.
func New(d *Deps) *Manager {
	must.NotNil("d", d)
	must.NotNil("d.Core", d.Core)
	must.NotNil("d.Coord", d.Coord)
	must.NotNil("d.Stores", d.Stores)
	return &Manager{
		d:        *d,
		dekCache: syncutil.NewTTLCache[string, []byte](d.DEKCacheTTL),
		log:      logfmt.Logger("multipart"),
	}
}

// Deps groups the multipart manager's constructor parameters: backend
// runtime, shared write coordinator, store surface, optional encryption and
// compression, the DEK-cache TTL, and the shared integrity config. Encryptor
// is nil when encryption is disabled and Codec when no codec is configured.
// Codec is supplied whether or not Compression.Enabled, matching the object
// manager: an assembled object is encoded only when both are set. With
// EnforceMinPartSize every non-final part must meet the S3 5 MiB floor.
type Deps struct {
	Core               Runtime
	Coord              *writepath.Coordinator
	Stores             Stores
	Encryptor          *encryption.Encryptor
	Codec              Codec
	Compression        config.CompressionConfig
	DEKCacheTTL        time.Duration
	IntegrityCfg       syncutil.Section[config.IntegrityConfig]
	EnforceMinPartSize bool
}

// Close stops the per-upload DEK cache eviction loop.
func (mp *Manager) Close() {
	if mp.dekCache != nil {
		mp.dekCache.Close()
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
	if mp.d.Encryptor != nil {
		_, wrappedDEK, kid, kerr := mp.d.Encryptor.GenerateAndWrapDEK(ctx)
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
	params := &core.CreateMultipartUploadParams{
		UploadID:      uploadID,
		ObjectKey:     key,
		ContentType:   req.ContentType,
		Metadata:      req.Metadata,
		EncryptionKey: encryptionKey,
		KeyID:         keyID,
		Tags:          req.Tags,
	}
	if err := mp.d.Coord.ClaimUploadTarget(ctx, span, operation, params); err != nil {
		return "", "", err
	}
	backendName := params.BackendName

	span.SetAttributes(telemetry.AttrBackendName.String(backendName))
	mp.d.Core.Acct().Operation(operation, backendName, start, nil)

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

	if !core.ValidPartNumber(partNumber) {
		err := badRequest("InvalidArgument", fmt.Sprintf("Part number must be between %d and %d", core.MinPartNumber, core.MaxPartNumber))
		observe.MarkSpanError(span, err.Message)
		return "", err
	}

	mu, err := mp.fetchScopedUpload(ctx, span, bucket, key, uploadID, operation)
	if err != nil {
		return "", err
	}

	be, err := mp.d.Core.GetBackend(mu.BackendName)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}

	// Checked against what the part will occupy, which for an encrypted upload
	// is the envelope rather than the bytes the client sent.
	if !mp.d.Core.Acct().Allow(mu.BackendName, s3op.UploadPart, 0, mp.physicalPartSize(mu, size)) {
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

	// The part claims the upload's backend before its bytes land, as every
	// write claims its target, and writes a path of its own, so a re-upload of
	// this number never shares bytes with the part it replaces.
	intent := writepath.NewCopyIntent(core.PartPath(uploadID, partNumber), mu.BackendName, uploadSize, form)
	if err := mp.d.Coord.ClaimCopyTarget(ctx, intent, core.AcceptForPart); err != nil {
		observe.RecordSpanError(span, err)
		if errors.Is(err, core.ErrTargetRefused) {
			return "", core.ErrInsufficientStorage
		}
		return "", err
	}
	storedETag, err := mp.d.Core.Put(ctx, operation, &s3be.PutRequest{
		Dst: s3be.Endpoint{Name: mu.BackendName, Backend: be}, Key: intent.StorageKey,
		Body: uploadBody, Size: uploadSize, ContentType: "application/octet-stream",
	})
	if err != nil {
		mp.d.Coord.Discard(ctx, intent, reasonPartUnrecorded)
		observe.RecordSpanError(span, err)
		return "", fmt.Errorf("failed to upload part: %w", err)
	}

	plaintextETag := etag.Hex(partDigest)
	replaced, err := mp.d.Stores.RecordPart(ctx, &core.RecordPartParams{
		UploadID: uploadID, PartNumber: partNumber, ETag: storedETag,
		PlaintextETag: plaintextETag, SizeBytes: uploadSize, Form: form, Intent: intent,
		CompletionStaleBefore: completionStaleBefore(),
	})
	// A part replaced by this one, or this one when its intent was lost, was
	// queued by the record's transaction.
	mp.d.Coord.SettleAfterResponse(ctx, replaced)
	if err != nil {
		mp.log.ErrorContext(ctx, "recordPart failed, discarding part object",
			"upload_id", uploadID, "part", partNumber, "error", err)
		mp.d.Coord.Discard(ctx, intent, reasonPartUnrecorded)
		observe.RecordSpanError(span, err)
		return "", fmt.Errorf("failed to record part: %w", err)
	}

	// The PUT was charged at what was sent, not at what the client handed
	// over: an encrypted part carries its own envelope, and a large upload
	// repeats that difference once per part.
	mp.d.Core.Acct().Operation(operation, mu.BackendName, start, nil)
	pobserve.UploadPartCompleted(ctx, span, mu.ObjectKey, mu.BackendName, uploadID, partNumber, size)
	// The client is given the MD5 of its own bytes, which is what S3 returns
	// and what it will send back in the completion manifest.
	return etag.Single(plaintextETag), nil
}

// physicalPartSize reports how many bytes a part of this size will occupy once
// encrypted. Parts are never compressed, so this is known before the read.
func (mp *Manager) physicalPartSize(mu *core.MultipartUpload, size int64) int64 {
	if mp.d.Encryptor == nil || !mu.Encrypted {
		return size
	}
	return mp.d.Encryptor.CiphertextSize(size)
}

// -------------------------------------------------------------------------
// READ-ONLY ACCESSORS
// -------------------------------------------------------------------------

// ListMultipartUploads returns active multipart uploads matching the given
// prefix, up to maxUploads results. Pass-through to the metadata store.
func (mp *Manager) ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]core.MultipartUpload, error) {
	return mp.d.Stores.ListMultipartUploads(ctx, prefix, maxUploads)
}

// GetParts returns all parts for a multipart upload, each reporting the ETag
// and size UploadPart handed the client rather than what the backend holds;
// the two differ once the stored part is an encryption envelope.
func (mp *Manager) GetParts(ctx context.Context, bucket, key, uploadID string) ([]core.MultipartPart, error) {
	parts, _, err := mp.ListParts(ctx, bucket, key, uploadID, 0, MaxPartCount)
	return parts, err
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
	// One extra row says whether the page is the last without a count query,
	// so a zero-size page still reports whether any parts lie past the marker.
	parts, err := mp.d.Stores.ListParts(ctx, uploadID, partNumberMarker, maxParts+1)
	if err != nil {
		return nil, false, err
	}
	parts, truncated := core.TrimPage(parts, maxParts)
	for i := range parts {
		presentToClient(&parts[i])
	}
	return parts, truncated, nil
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
	unwrapped, err := mp.d.Encryptor.Provider().UnwrapDEK(ctx, wrappedDEK, mu.KeyID)
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
	sealed, err := storedform.Seal(ctx, mp.d.Encryptor, body, size, &storedform.DataKey{DEK: dek, WrappedDEK: wrappedDEK, KeyID: mu.KeyID})
	if err != nil {
		return nil, 0, nil, err
	}
	return sealed.Body, sealed.Size, sealed.Form, nil
}

// prepareUploadPartBody returns the body, size and stored form for one part,
// encrypted under the upload's cached data key when the upload is encrypted.
func (mp *Manager) prepareUploadPartBody(ctx context.Context, mu *core.MultipartUpload, body io.Reader, size int64) (io.Reader, int64, *core.StoredForm, error) {
	if mp.d.Encryptor == nil || !mu.Encrypted {
		return body, size, nil, nil
	}
	out, ciphertextSize, form, err := mp.encryptWithUploadDEK(ctx, mu, body, size)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("encrypt part: %w", err)
	}
	return out, ciphertextSize, form, nil
}

// fetchScopedUpload looks up the multipart upload and verifies it belongs to
// the (bucket, key) the request URL implies. Missing and out-of-scope rows
// return the same NoSuchUpload error, so callers cannot probe for upload IDs
// across buckets. Errors are recorded on span, the operation's own span.
func (mp *Manager) fetchScopedUpload(ctx context.Context, span trace.Span, bucket, key, uploadID string, operation s3op.Operation) (*core.MultipartUpload, error) {
	mu, err := mp.d.Stores.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return nil, mp.d.Core.ClassifyWriteError(span, operation, err)
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

// collectRequestedParts loads every part for uploadID and returns the ones the
// manifest names, in manifest order, which validateManifestShape has already
// made ascending. A requested part that was never uploaded fails the
// completion, naming every one missing.
func (mp *Manager) collectRequestedParts(ctx context.Context, span trace.Span, uploadID string, partNumbers []int) ([]core.MultipartPart, error) {
	allParts, err := mp.d.Stores.ListParts(ctx, uploadID, 0, MaxPartCount)
	if err != nil {
		observe.RecordSpanError(span, err)
		return nil, err
	}
	byNumber := make(map[int]*core.MultipartPart, len(allParts))
	for i := range allParts {
		byNumber[allParts[i].PartNumber] = &allParts[i]
	}
	parts := make([]core.MultipartPart, 0, len(partNumbers))
	var missing []int
	for _, pn := range partNumbers {
		if p, ok := byNumber[pn]; ok {
			parts = append(parts, *p)
		} else {
			missing = append(missing, pn)
		}
	}
	if len(missing) > 0 {
		msg := "parts not uploaded: " + formatPartNumbers(missing)
		observe.MarkSpanError(span, msg)
		return nil, badRequest(codeInvalidPart, msg)
	}
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
	ctx, span := telemetry.StartSpan(ctx, spanPrefix+s3op.AbortMultipartUpload.String(),
		telemetry.AttrUploadID.String(mu.UploadID),
	)
	defer span.End()
	const operation = s3op.AbortMultipartUpload
	start := time.Now()
	uploadID := mu.UploadID

	if _, err := mp.d.Core.GetBackend(mu.BackendName); err != nil {
		observe.RecordSpanError(span, err)
		return err
	}

	// The upload and its parts go and the parts' bytes are queued in one
	// transaction, so an abort that dies before the deletes run leaves them
	// to the cleanup worker rather than on the backend with nothing naming them.
	parts, err := mp.d.Stores.DeleteMultipartUpload(ctx, uploadID, reasonAbortPart, completionStaleBefore())
	if err != nil {
		observe.RecordSpanError(span, err)
		return err
	}
	mp.d.Coord.SettleAfterResponse(ctx, parts)
	mp.forgetUploadDEK(uploadID)

	// The abort itself sends no request: parts are plain objects, so the only
	// backend calls are the part deletes, charged where they are sent.
	mp.d.Core.Acct().Operation(operation, mu.BackendName, start, nil)

	pobserve.MultipartAborted(ctx, span, uploadID, mu.ObjectKey, mu.BackendName, len(parts))
	return nil
}

// The cleanup-queue reasons for the parts an abort and a completion remove,
// and for a part upload that was never recorded.
const (
	reasonAbortPart      = "abort_part_cleanup"
	reasonCompletePart   = "complete_part_cleanup"
	reasonPartUnrecorded = "orphan_part_record_failed"
)

// CleanupStaleMultipartUploads aborts multipart uploads older than the given
// duration, as one multipart_cleanup pass. Run periodically to prevent quota
// leaks from abandoned uploads.
func (mp *Manager) CleanupStaleMultipartUploads(ctx context.Context, olderThan time.Duration) batch.Summary {
	sum, _ := cycle.Run(ctx, cycle.Pass{Workload: audit.WorkloadMultipartCleanup}, func(ctx context.Context) (batch.Summary, error) {
		return mp.abortMatching(ctx, core.MultipartUploadFilter{CreatedBefore: time.Now().Add(-olderThan)}), nil
	})
	return sum
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
			return mp.d.Stores.ScanMultipartUploads(ctx, filter, limit, after)
		},
		CursorOf: func(mu core.MultipartUpload) string { return mu.UploadID },
	}
	runner := batch.Runner[core.MultipartUpload]{Concurrency: 1}
	total, stop, err := pager.RunPages(ctx, runner, func(ctx context.Context, mu core.MultipartUpload) batch.ItemResult {
		mp.log.InfoContext(ctx, "aborting multipart upload", "upload_id", mu.UploadID, "key", mu.ObjectKey, "backend", mu.BackendName)
		if err := mp.abortByMultipartRow(ctx, &mu); err != nil {
			mp.log.ErrorContext(ctx, "failed to abort multipart upload", "upload_id", mu.UploadID, "error", err)
			return batch.Failed()
		}
		return batch.Succeeded()
	})
	if stop == batch.Errored {
		mp.log.ErrorContext(ctx, "failed to list multipart uploads",
			"backend", filter.Backend, "created_before", filter.CreatedBefore, "error", err)
	}
	return total
}

// CompleteMultipartUpload streams the parts into one assembled object, records
// its location, and removes the parts. It holds the upload's completion mark
// while it runs, so a part upload, an abort or a second completion is refused
// with 409 OperationAborted instead of deleting parts the assembly is reading.
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
	started, err := mp.d.Stores.BeginMultipartCompletion(ctx, uploadID, completionStaleBefore())
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}
	if !started {
		// An upload aborted since the fetch is gone, not held.
		if _, err := mp.d.Stores.GetMultipartUpload(ctx, uploadID); err != nil {
			return "", mp.d.Core.ClassifyWriteError(span, operation, err)
		}
		observe.MarkSpanError(span, "another CompleteMultipartUpload in flight")
		return "", core.ErrCompletionInProgress
	}

	return mp.withCompletionMark(ctx, uploadID, func(ctx context.Context) (string, error) {
		return mp.completeMultipartUploadLocked(ctx, span, operation, mu, manifest, start)
	})
}

// The completion mark goes stale once it has not been renewed for
// completionGrace, which frees the upload of a completion that died.
// completionRenewEvery renews it well inside that window.
const (
	completionGrace      = time.Minute
	completionRenewEvery = completionGrace / 4
)

// completionStaleBefore is the renewal time before which a completion mark
// belongs to a completion that died.
func completionStaleBefore() time.Time {
	return time.Now().Add(-completionGrace)
}

// withCompletionMark runs complete while renewing the upload's completion
// mark, and clears the mark when complete returns. A renewal that fails or
// finds the mark gone cancels complete, because the upload may no longer be
// protected from deletes of the parts it is reading.
func (mp *Manager) withCompletionMark(ctx context.Context, uploadID string, complete func(context.Context) (string, error)) (string, error) {
	workCtx, cancel := context.WithCancelCause(ctx)
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		syncutil.Every(workCtx, completionRenewEvery, func(ctx context.Context) {
			held, err := mp.d.Stores.RenewMultipartCompletion(ctx, uploadID)
			if err == nil && !held {
				err = errCompletionMarkLost
			}
			if err != nil {
				cancel(fmt.Errorf("renew completion mark: %w", err))
			}
		})
	}()

	etag, err := complete(workCtx)
	cancel(nil)
	<-renewed
	if endErr := mp.d.Stores.EndMultipartCompletion(context.WithoutCancel(ctx), uploadID); endErr != nil {
		mp.log.WarnContext(ctx, "failed to clear multipart completion mark", "upload_id", uploadID, "error", endErr)
	}
	return etag, err
}

// errCompletionMarkLost reports a renewal that found the completion mark gone.
var errCompletionMarkLost = errors.New("completion mark is gone")

// completeMultipartUploadLocked assembles mu while CompleteMultipartUpload
// holds the completion mark. The order is validate, assemble, commit, then drop
// the parts: any failure before the commit leaves the parts and the upload row
// intact so the client can retry, and the stale-multipart sweep reaps an
// upload that is abandoned after a failure.
func (mp *Manager) completeMultipartUploadLocked(
	ctx context.Context,
	span trace.Span,
	operation s3op.Operation,
	mu *core.MultipartUpload,
	manifest []core.CompletePart,
	start time.Time,
) (string, error) {
	uploadID := mu.UploadID
	be, err := mp.d.Core.GetBackend(mu.BackendName)
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
	// under the same completion mark, so a part replaced between validation and
	// assembly cannot slip through.
	if err := validateManifestAgainstStored(manifest, parts, mp.d.EnforceMinPartSize); err != nil {
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

	pr, pipeCancel := mp.streamPartsThroughPipe(ctx, s3be.Endpoint{Name: mu.BackendName, Backend: be}, uploadID, parts)
	defer pipeCancel()

	// Tee the plaintext stream through SHA-256 when integrity is on so
	// the assembled object lands with a content_hash matching the
	// regular PutObject path. Without this, the scrubber cannot verify
	// multipart-completed objects.
	hasher := mp.d.IntegrityCfg.Config().Hasher()
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

	uploadBody, uploadSize, form, err := mp.buildAssembledUpload(ctx, span, mu, stored, totalPlaintextSize)
	if err != nil {
		return "", err
	}

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
	intent := writepath.NewPendingIntent(mu.ObjectKey, uploadSize, form, identity, mu.Tags)
	if err := mp.d.Coord.ClaimWriteTarget(ctx, span, operation, intent, []string{mu.BackendName}); err != nil {
		return "", err
	}

	// The pipe is fed by the part downloads, so the PUT's timeout covers the
	// whole stream-parts, assemble and PUT pipeline. Assembled under the
	// intent's own storage key, so a double completion or a concurrent write to
	// the key leaves two distinct objects instead of two writers racing at one
	// path. The backend's ETag describes the stored bytes and is discarded in
	// favor of the composite.
	_, err = mp.d.Core.Put(ctx, s3op.PutObject, &s3be.PutRequest{
		Dst: s3be.Endpoint{Name: mu.BackendName, Backend: be}, Key: intent.StorageKey,
		Body: uploadBody, Size: uploadSize, ContentType: mu.ContentType, Metadata: mu.Metadata,
	})
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

	if hasher != nil {
		form = storedform.WithContentHash(form, etag.Hex(hasher))
	}
	if identity.ETag == "" {
		identity.ETag = etag.Single(etag.Hex(assemblyDigest))
	}

	// The create call's tags land in the same transaction, so a completed upload
	// is never briefly untagged. The bytes are charged from the size the ledger
	// records.
	if err := mp.d.Coord.RecordObjectAndPromoteIntent(ctx, span, operation, intent.RecordFor(form)); err != nil {
		return "", err
	}

	// Only now is the object durably committed, so the source parts are safe
	// to drop. They go while the mark is still held, so a retried completion
	// cannot assemble them a second time.
	mp.cleanupCompletedUpload(ctx, span, uploadID)

	// The part GETs, the assembled PUT and the part deletes were each charged
	// where they were sent.
	mp.d.Core.Acct().Operation(operation, mu.BackendName, start, nil)

	pobserve.MultipartCompleted(ctx, span, mu.ObjectKey, mu.BackendName, uploadID, totalPlaintextSize, len(parts))
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
	return core.NewObjectIdentity(composite, mu.ContentType, mu.Metadata), nil
}

// cleanupCompletedUpload removes the multipart_uploads row, queueing its parts'
// bytes in the same transaction, then deletes them and drops the cached DEK of
// a durably committed upload. Call it only after the commit: on a failure path
// it would destroy the parts a retry needs. The completion mark is the
// caller's own, so the delete ignores any mark renewed before now. A removal
// that fails leaves the upload to the stale-multipart sweep, which aborts it.
func (mp *Manager) cleanupCompletedUpload(ctx context.Context, span trace.Span, uploadID string) {
	parts, err := mp.d.Stores.DeleteMultipartUpload(ctx, uploadID, reasonCompletePart, time.Now())
	if err != nil {
		span.RecordError(err)
	}
	mp.d.Coord.SettleAfterResponse(ctx, parts)
	mp.forgetUploadDEK(uploadID)
}

// sumPlaintextSize returns the total byte count the client uploaded across
// parts, whatever each part was stored as.
func sumPlaintextSize(parts []core.MultipartPart) int64 {
	var total int64
	for i := range parts {
		total += parts[i].ClientSize(parts[i].SizeBytes)
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

// compressAssembly encodes the assembled plaintext when compression applies and
// reports what the PUT should send. The encoding is buffered because a backend
// PUT declares its size up front. An encoding that misses min_ratio is decoded
// back out of that buffer, since the part pipe delivers the plaintext only once
// and re-reading the parts would charge their egress a second time.
func (mp *Manager) compressAssembly(src io.Reader, totalPlaintextSize int64) (*assembledBody, error) {
	if !storedform.ShouldCompress(&mp.d.Compression, mp.d.Codec, totalPlaintextSize) {
		return &assembledBody{body: src, size: totalPlaintextSize}, nil
	}

	enc, err := storedform.Compress(mp.d.Codec, src, totalPlaintextSize, mp.d.Compression.MinRatio)
	if err != nil {
		return nil, fmt.Errorf("compress assembled object: %w", err)
	}
	a := &assembledBody{encoded: enc.Body}
	reader := enc.Body.Reader()
	if enc.Worth {
		a.body, a.size, a.compressed = reader, enc.Size, true
		return a, nil
	}

	plain, err := mp.d.Codec.Decompress(reader)
	if err != nil {
		a.cleanup()
		return nil, fmt.Errorf("decode discarded encoding: %w", err)
	}
	a.body, a.size, a.decoded = plain, totalPlaintextSize, plain
	return a, nil
}

// buildAssembledUpload returns the body the assembly PUT sends and the form
// describing it. With encryption configured it encrypts the stored bytes under
// the upload-level DEK from mu, so the assembled object shares its DEK with
// every part.
func (mp *Manager) buildAssembledUpload(
	ctx context.Context,
	span trace.Span,
	mu *core.MultipartUpload,
	stored *assembledBody,
	totalPlaintextSize int64,
) (io.Reader, int64, *core.StoredForm, error) {
	out, size := stored.body, stored.size
	var form *core.StoredForm
	if mp.d.Encryptor != nil {
		var err error
		out, size, form, err = mp.encryptWithUploadDEK(ctx, mu, stored.body, stored.size)
		if err != nil {
			observe.RecordSpanError(span, err)
			return nil, 0, nil, fmt.Errorf("encrypt final object: %w", err)
		}
	}
	if stored.compressed {
		form = storedform.Describe(form, mp.d.Compression.Level, totalPlaintextSize)
	}
	return out, size, form, nil
}

// -------------------------------------------------------------------------
// PART STREAMING
// -------------------------------------------------------------------------

// streamPartsThroughPipe spawns a goroutine that streams the parts from src
// in order, as plaintext, into the returned reader. The caller must invoke the
// returned cancel func to stop in-flight backend reads when assembly fails
// downstream (e.g. the final PutObject errors out).
func (mp *Manager) streamPartsThroughPipe(
	ctx context.Context,
	src s3be.Endpoint,
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
		if err := mp.streamParts(pipeCtx, src, bw, uploadID, parts); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := bw.Flush(); err != nil {
			pw.CloseWithError(fmt.Errorf("failed to flush multipart stream: %w", err))
		}
	}()

	return pr, pipeCancel
}

// partsAhead is how many part GETs a completion keeps open past the part it is
// copying.
const partsAhead = 4

// openedPart is a part whose GET has been answered, waiting for its turn to be
// copied.
type openedPart struct {
	part    *core.MultipartPart
	result  *s3be.GetObjectResult
	start   func()
	release context.CancelFunc
	err     error
}

// close closes the part's body and frees its GET.
func (o openedPart) close() {
	if o.err == nil {
		_ = o.result.Body.Close()
		o.release()
	}
}

// streamParts copies each part from src into bw in order while the GETs of the
// next partsAhead parts wait for their responses, so a backend's wait for a
// first byte overlaps the copies instead of adding to them.
func (mp *Manager) streamParts(ctx context.Context, src s3be.Endpoint, bw io.Writer, uploadID string, parts []core.MultipartPart) error {
	ahead := make([]<-chan openedPart, 0, partsAhead)
	next := 0
	openMore := func() {
		for ; len(ahead) < partsAhead && next < len(parts); next++ {
			ahead = append(ahead, mp.openAhead(ctx, src, uploadID, &parts[next]))
		}
	}
	defer func() {
		for _, opening := range ahead {
			go mp.discardPart(ctx, src.Name, opening)
		}
	}()
	openMore()
	for len(ahead) > 0 {
		opened := <-ahead[0]
		ahead = ahead[1:]
		openMore()
		if err := mp.streamOnePart(ctx, src.Name, bw, opened); err != nil {
			return err
		}
	}
	return nil
}

// openPart sends one part's GET and returns it once answered. The part's body
// is timed from when it is copied, not from when it was opened.
func (mp *Manager) openPart(ctx context.Context, src s3be.Endpoint, uploadID string, part *core.MultipartPart) openedPart {
	result, start, release, err := mp.d.Core.OpenGet(ctx, src.Name, src.Backend, part.Path(uploadID))
	return openedPart{part: part, result: result, start: start, release: release, err: err}
}

// openAhead runs openPart in the background and delivers the part when its GET
// is answered. A GET that panics is delivered as the part's error.
func (mp *Manager) openAhead(ctx context.Context, src s3be.Endpoint, uploadID string, part *core.MultipartPart) <-chan openedPart {
	opening := make(chan openedPart, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				opening <- openedPart{part: part, err: fmt.Errorf("multipart assembly panic: %v", r)}
			}
		}()
		opening <- mp.openPart(ctx, src, uploadID, part)
	}()
	return opening
}

// discardPart frees a part opened for an assembly that failed before reaching
// it, charging the call its GET made.
func (mp *Manager) discardPart(ctx context.Context, srcName string, opening <-chan openedPart) {
	opened := <-opening
	if opened.err == nil {
		mp.d.Core.Acct().APICall(ctx, s3op.GetObject, srcName)
		opened.close()
	}
}

// streamOnePart copies an opened part into bw, decrypting it when it was
// stored encrypted, and charges its egress once all of it has been read. The
// part's GET is freed before returning, and errors name the part.
func (mp *Manager) streamOnePart(ctx context.Context, srcName string, bw io.Writer, opened openedPart) error {
	part := opened.part
	if opened.err != nil {
		return fmt.Errorf("failed to read part %d: %w", part.PartNumber, opened.err)
	}
	defer opened.close()
	opened.start()
	if err := copyPart(ctx, mp.d.Encryptor, bw, opened.result.Body, part); err != nil {
		mp.d.Core.Acct().APICall(ctx, s3op.GetObject, srcName)
		return err
	}
	mp.d.Core.Acct().Egress(ctx, s3op.GetObject, srcName, part.SizeBytes)
	return nil
}

// copyPart decrypts one stored part when it was stored encrypted and copies
// the plaintext into bw.
func copyPart(ctx context.Context, enc *encryption.Encryptor, bw io.Writer, stored io.Reader, part *core.MultipartPart) error {
	plain, err := storedform.Unseal(ctx, enc, stored, &part.StoredForm)
	if err != nil {
		if errors.Is(err, encryption.ErrInvalidKeyData) {
			return fmt.Errorf("unpack part %d key: %w", part.PartNumber, err)
		}
		return fmt.Errorf("open part %d: %w", part.PartNumber, err)
	}
	if _, err := bufpool.Copy(bw, plain); err != nil {
		return fmt.Errorf("failed to stream part %d: %w", part.PartNumber, err)
	}
	return nil
}

// CountActiveMultipartUploads returns the number of in-progress uploads whose
// key falls under bucketPrefix. Lives here rather than on a facade because
// this is the type that already holds the multipart store.
func (mp *Manager) CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error) {
	return mp.d.Stores.CountActiveMultipartUploads(ctx, bucketPrefix)
}

// -------------------------------------------------------------------------------
// Object Manager - Write Operations (PUT, COPY, DELETE)
//
// Author: Alex Freidah
//
// PutObject, CopyObject, DeleteObject, DeleteObjects.
// read failover across replicas, broadcast reads during degraded mode, and
// usage limit enforcement on reads and writes. DeleteObjects provides batch
// deletion with concurrent backend I/O.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	pobserve "github.com/afreidah/s3-orchestrator/internal/proxy/observe"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// -------------------------------------------------------------------------
// OBJECT CRUD
// -------------------------------------------------------------------------

// PutObject uploads an object to the first backend with available quota.
// If the upload fails, it retries on remaining eligible backends before
// returning an error to the caller (write failover).
func (o *Manager) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error) {
	const operation = "PutObject"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
		telemetry.AttrObjectKey.String(key),
		telemetry.AttrObjectSize.Int64(size),
	)
	defer span.End()

	eligible := o.core.EligibleForWrite(1, 0, size)
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "write").Inc()
		observe.MarkSpanError(span, "usage limits exceeded on all backends")
		return "", core.ErrInsufficientStorage
	}

	mbody, contentHash, err := o.bufferPutBody(span, body, size)
	if err != nil {
		return "", err
	}
	defer mbody.Cleanup()

	// DEK caching: encryptForPut wraps a fresh DEK on first call and
	// reuses it on retries with a new base nonce, sparing the KeyProvider
	// during failover storms.
	var dekState putEncryptState
	var failedBackends []string
	var lastErr error

	for len(eligible) > 0 {
		res := o.attemptPutOnBackend(ctx, span, &putAttemptRequest{
			operation:   operation,
			key:         key,
			body:        mbody,
			size:        size,
			contentType: contentType,
			metadata:    metadata,
			contentHash: contentHash,
			dekState:    &dekState,
			eligible:    eligible,
		})
		if res.fatalErr != nil {
			return "", res.fatalErr
		}
		if res.putErr == nil {
			o.finalizePutSuccess(ctx, span, &putSuccessRequest{
				operation:      operation,
				key:            key,
				backendName:    res.backend,
				size:           size,
				start:          start,
				failedBackends: failedBackends,
			})
			return res.etag, nil
		}
		lastErr = res.putErr
		failedBackends = append(failedBackends, res.backend)
		eligible = withoutBackend(eligible, res.backend)
		o.log.WarnContext(ctx, "PutObject: backend write failed, trying next",
			"key", key, "failed_backend", res.backend, "error", res.putErr,
			"remaining_backends", len(eligible))
	}

	observe.RecordSpanError(span, lastErr)
	return "", lastErr
}

// putAttemptResult conveys the outcome of one backend PUT attempt back to
// the failover loop. A non-nil fatalErr terminates the call. A non-nil
// putErr signals a backend-side failure that should drop the chosen
// backend and retry on the remainder.
type putAttemptResult struct {
	backend  string
	etag     string
	fatalErr error
	putErr   error
}

// bufferPutBody materializes the request body into a seekable form
// (memory for small payloads, tempfile above materializeMemThreshold)
// so failover retries can replay the plaintext without holding the
// full body on the heap. When integrity verification is enabled, the
// SHA-256 is computed during the same single buffering pass via
// io.MultiWriter so the body is not re-scanned after materialization.
//
// Returns the materialized body, the content hash (empty when
// integrity verification is disabled), and a cleanup the caller must
// invoke once the upload settles (safe to defer in every code path).
func (o *Manager) bufferPutBody(span trace.Span, body io.Reader, size int64) (*materializedBody, string, error) {
	var hasher hash.Hash
	icfg := o.integrityCfg.Load()
	if icfg != nil && icfg.Enabled {
		hasher = newSHA256()
	}
	mb, err := newMaterializedBody(body, size, hasher)
	if err != nil {
		observe.RecordSpanError(span, err)
		return nil, "", fmt.Errorf("buffer request body: %w", err)
	}
	return mb, sha256Hex(hasher), nil
}

// putAttemptRequest bundles the per-attempt arguments to
// attemptPutOnBackend so the helper signature stays under the
// parameter-count limit.
type putAttemptRequest struct {
	operation   string
	key         string
	body        *materializedBody
	size        int64
	contentType string
	metadata    map[string]string
	contentHash string
	dekState    *putEncryptState
	eligible    []string
}

// attemptPutOnBackend performs one backend PUT attempt: select a
// destination, prepare the payload (encrypt/hash), insert a pending
// intent, upload, then promote the intent on success.
func (o *Manager) attemptPutOnBackend(ctx context.Context, span trace.Span, req *putAttemptRequest) putAttemptResult {
	backendName, err := o.coord.SelectBackendForWrite(ctx, req.size, req.eligible)
	if err != nil {
		return putAttemptResult{fatalErr: o.core.ClassifyWriteError(span, req.operation, err)}
	}
	span.SetAttributes(telemetry.AttrBackendName.String(backendName))

	be, err := o.core.GetBackend(backendName)
	if err != nil {
		observe.RecordSpanError(span, err)
		return putAttemptResult{backend: backendName, fatalErr: err}
	}

	uploadBody, uploadSize, enc, err := o.buildPutPayload(ctx, req.body, req.size, req.contentHash, req.dekState)
	if err != nil {
		observe.RecordSpanError(span, err)
		return putAttemptResult{backend: backendName, fatalErr: err}
	}

	// Insert the pending intent before the backend PUT so a metadata
	// commit failure after the bytes land has a recovery breadcrumb: the
	// pending reaper promotes the intent on a later tick instead of the
	// old failure path silently deleting the just-written copy.
	intentID, err := o.coord.InsertPendingIntent(ctx, req.key, backendName, uploadSize, enc)
	if err != nil {
		observe.RecordSpanError(span, err)
		return putAttemptResult{backend: backendName, fatalErr: err}
	}

	bctx, bcancel := o.core.WithTimeout(ctx)
	etag, err := be.PutObject(bctx, req.key, uploadBody, uploadSize, req.contentType, req.metadata)
	bcancel()
	if err != nil {
		o.core.Acct().APICall(backendName)
		// Leave the pending row for the reaper. A backend PUT error does
		// not reliably mean the bytes are absent: the response could have
		// been lost mid-flight, so the reaper HEADs the backend on its
		// next tick and either promotes or drops the intent.
		return putAttemptResult{backend: backendName, putErr: err}
	}

	// Drain-race close: a drain that started after EligibleForWrite ran
	// could have flipped this backend to draining while the backend PUT
	// was in flight. If finalizeDrain's DeleteBackendData runs after the
	// commit below, the just-promoted row gets wiped and the physical
	// bytes are orphaned. Re-check before the commit so we land on a
	// different backend instead; the pending intent stays for the
	// reaper, which HEADs the backend, sees no object (we just deleted
	// the bytes), and drops the intent.
	if o.core.IsDraining(backendName) {
		o.log.WarnContext(ctx, "drain started mid-write; aborting commit on draining backend",
			"key", req.key, "backend", backendName)
		telemetry.DrainRaceAbortedTotal.Inc()
		o.coord.RecoverFromRecordFailure(ctx, be, backendName, req.key, "drain_race_aborted", uploadSize)
		return putAttemptResult{backend: backendName, putErr: errDrainRaceAborted}
	}

	if err := o.coord.RecordObjectAndPromoteIntent(ctx, span, req.key, backendName, uploadSize, enc, intentID); err != nil {
		return putAttemptResult{backend: backendName, fatalErr: err}
	}
	return putAttemptResult{backend: backendName, etag: etag}
}

// errDrainRaceAborted is the sentinel putErr the attemptPutOnBackend
// drain-race close returns so the outer failover loop drops the
// draining backend from the eligible set and retries elsewhere
// instead of treating the abort as a generic backend failure.
var errDrainRaceAborted = errors.New("aborted: drain started mid-write")

// buildPutPayload prepares the upload body and EncryptionMeta for a
// single attempt. The materialized body's Reader() rewinds on every
// call so encryption and unencrypted paths both replay from offset 0
// across failover retries. Encryption layering, when enabled, runs
// through encryptForPut so the wrapped DEK is reused across retries.
func (o *Manager) buildPutPayload(
	ctx context.Context,
	body *materializedBody,
	size int64,
	contentHash string,
	dekState *putEncryptState,
) (io.Reader, int64, *core.EncryptionMeta, error) {
	plain, err := body.Reader()
	if err != nil {
		return nil, 0, nil, err
	}
	if o.encryptor != nil {
		uploadBody, uploadSize, enc, err := encryptForPut(ctx, o.encryptor, plain, size, dekState)
		if err != nil {
			return nil, 0, nil, err
		}
		enc.ContentHash = contentHash
		return uploadBody, uploadSize, enc, nil
	}
	var enc *core.EncryptionMeta
	if contentHash != "" {
		enc = &core.EncryptionMeta{ContentHash: contentHash}
	}
	return plain, size, enc, nil
}

// putSuccessRequest bundles the metadata that finalizePutSuccess emits
// so the helper signature stays under the parameter-count limit.
type putSuccessRequest struct {
	operation      string
	key            string
	backendName    string
	size           int64
	start          time.Time
	failedBackends []string
}

// finalizePutSuccess emits success metrics, audit log, and an event
// notification for a successful PutObject. Records failover spans when
// retries occurred.
func (o *Manager) finalizePutSuccess(ctx context.Context, span trace.Span, req *putSuccessRequest) {
	o.core.Acct().PutSuccess(req.operation, req.backendName, req.size, req.start)
	if len(req.failedBackends) > 0 {
		for _, fb := range req.failedBackends {
			telemetry.WriteFailoverTotal.WithLabelValues(req.operation, fb, req.backendName).Inc()
		}
		span.SetAttributes(telemetry.AttrWriteFailover.Bool(true))
		span.SetAttributes(telemetry.AttrFailoverAttempts.Int(len(req.failedBackends)))
	}
	pobserve.PutCompleted(ctx, span, req.key, req.backendName, req.size)
	o.invalidateObjectCaches(req.key)
}

// withoutBackend returns eligible with name removed in original order.
func withoutBackend(eligible []string, name string) []string {
	remaining := make([]string, 0, len(eligible)-1)
	for _, n := range eligible {
		if n != name {
			remaining = append(remaining, n)
		}
	}
	return remaining
}

// -------------------------------------------------------------------------
// COPY
// -------------------------------------------------------------------------

// headSourceForCopy walks the source's known locations until one HEAD
// succeeds (skipping over-limit and unknown backends), and returns its
// metadata plus optional encryption descriptor. ok=false signals that no
// copy could be reached.
func (o *Manager) headSourceForCopy(
	ctx context.Context,
	sourceKey string,
	locations []core.ObjectLocation,
) (int64, string, map[string]string, *core.EncryptionMeta, bool) {
	for i := range locations {
		if !o.core.Usage().WithinLimits(locations[i].BackendName, 1, 0, 0) {
			continue
		}
		be, ok := o.core.Backends()[locations[i].BackendName]
		if !ok {
			continue
		}
		bctx, bcancel := o.core.WithTimeout(ctx)
		headResult, err := be.HeadObject(bctx, sourceKey)
		bcancel()
		if err != nil {
			continue
		}
		var srcEnc *core.EncryptionMeta
		if locations[i].Encrypted {
			srcEnc = &core.EncryptionMeta{
				Encrypted:     true,
				EncryptionKey: locations[i].EncryptionKey,
				KeyID:         locations[i].KeyID,
				PlaintextSize: locations[i].PlaintextSize,
				ContentHash:   locations[i].ContentHash,
			}
		}
		return headResult.Size, headResult.ContentType, headResult.Metadata, srcEnc, true
	}
	return 0, "", nil, nil, false
}

// CopyObject copies an object from sourceKey to destKey. Materializes
// the source body into a seekable buffer  -  in-memory for small
// objects, a self-unlinking tempfile above materializeMemThreshold
// -  before handing it to the destination PutObject. A non-seekable
// body would force the AWS SDK onto its streaming-unsigned-payload
// signing path, which uses chunked transfer encoding and drops
// Content-Length; S3 implementations that require Content-Length
// (notably OCI) then reject the upload with HTTP 411. Supports
// cross-backend copies and read failover from replicas.
func (o *Manager) CopyObject(ctx context.Context, sourceKey, destKey string) (string, error) {
	const operation = "CopyObject"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
		attribute.String("s3o.source_key", sourceKey),
		attribute.String("s3o.dest_key", destKey),
	)
	defer span.End()

	locations, err := o.stores.GetAllObjectLocations(ctx, sourceKey)
	if err != nil {
		if errors.Is(err, core.ErrObjectNotFound) {
			observe.MarkSpanError(span, "source object not found")
			return "", err
		}
		return "", o.core.ClassifyWriteError(span, operation, err)
	}

	size, contentType, metadata, srcEnc, ok := o.headSourceForCopy(ctx, sourceKey, locations)
	if !ok {
		err := fmt.Errorf("failed to head source object from any copy")
		observe.RecordSpanError(span, err)
		return "", err
	}
	span.SetAttributes(telemetry.AttrObjectSize.Int64(size))

	destBackendName, err := o.coord.SelectWriteTarget(ctx, span, operation, size)
	if err != nil {
		return "", err
	}
	destBackend, err := o.core.GetBackend(destBackendName)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}

	// Same-backend fast path: when a source replica lives on the chosen
	// destination backend and that backend supports server-side copy,
	// skip the materialize+PUT round trip. Falls back to the slow path
	// on ErrCopyNotSupported or any native-call backend error. A
	// post-copy failure (e.g., metadata record failure) surfaces to the
	// caller because the bytes are already on the destination; falling
	// back would copy them a second time.
	if sameBackendCopyEligible(locations, destBackendName) {
		req := &nativeCopyRequest{
			span:            span,
			destBackend:     destBackend,
			sourceKey:       sourceKey,
			destKey:         destKey,
			destBackendName: destBackendName,
			size:            size,
			contentType:     contentType,
			metadata:        metadata,
			srcEnc:          srcEnc,
			start:           start,
		}
		if etag, handled, nerr := o.tryNativeCopy(ctx, req); handled {
			return etag, nerr
		}
	}

	src, err := o.materializeCopySource(ctx, sourceKey, size, locations)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}
	defer src.cleanup()

	// Seekable body keeps the SDK on UNSIGNED-PAYLOAD signing so
	// Content-Length survives to the backend. The dest PUT goes
	// through the centralized backend-timeout policy so a stalled
	// destination cannot tie up the request past the configured
	// backend_timeout (#882).
	wctx, wcancel := o.core.WithTimeout(ctx)
	defer wcancel()
	etag, err := destBackend.PutObject(wctx, destKey, src.body, size, contentType, metadata)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", fmt.Errorf("failed to write destination: %w", err)
	}

	// --- Record destination location and update quota ---
	// Preserve encryption metadata: ciphertext is copied as-is so the
	// destination keeps the same wrapped DEK and key ID.
	if err := o.coord.RecordObjectOrCleanup(ctx, span, destBackend, destKey, destBackendName, size, srcEnc); err != nil {
		return "", err
	}

	srcName := src.sourceBackend
	o.core.Acct().Operation(operation, destBackendName, start, nil)
	o.core.Acct().Egress(srcName, size)         // source: Get
	o.core.Acct().Ingress(destBackendName, size) // dest: Put

	pobserve.CopyCompleted(ctx, span, sourceKey, destKey, srcName, destBackendName, size)
	o.invalidateObjectCaches(destKey)
	return etag, nil
}

// sameBackendCopyEligible reports whether the source has at least one
// replica on destBackendName. The orchestrator only triggers the
// server-side copy fast path when both keys live on the same backend
// because no backend can satisfy a CopySource that points at a
// foreign bucket.
func sameBackendCopyEligible(locations []core.ObjectLocation, destBackendName string) bool {
	for i := range locations {
		if locations[i].BackendName == destBackendName {
			return true
		}
	}
	return false
}

// nativeCopyRequest bundles tryNativeCopy's inputs so its signature
// stays under the parameter-count limit. Mirrors the putSuccessRequest
// pattern used by finalizePutSuccess in this file.
type nativeCopyRequest struct {
	span            trace.Span
	destBackend     s3be.ObjectBackend
	sourceKey       string
	destKey         string
	destBackendName string
	size            int64
	contentType     string
	metadata        map[string]string
	srcEnc          *core.EncryptionMeta
	start           time.Time
}

// tryNativeCopy attempts a server-side CopyObject on req.destBackend
// and, on success, records the destination location, updates
// accounting, and emits the completion observability. Returns:
//
//   - (etag, true, nil):  native copy + record both succeeded (the
//                         success may have been confirmed via the
//                         HEAD-probe recovery path; either way the
//                         caller treats this as a successful copy)
//   - (_, true, err):     native copy succeeded but a post-step failed;
//                         bytes are already on the destination, so the
//                         caller MUST NOT fall back (that would copy
//                         the bytes a second time)
//   - (_, false, nil):    backend does not support native copy, or the
//                         native call failed AND a HEAD probe confirmed
//                         the destination is not in the expected state;
//                         caller falls back to the materialized copy path
//
// Native copy accounting differs from the materialized path: one API
// call against the destination backend with no egress and no ingress,
// because the bytes never traverse the orchestrator's network.
//
// On a non-capability native-copy error, the destination is HEAD-probed
// before the function decides whether to surface the error or fall back.
// This guards against the ambiguous case where the backend completed the
// copy server-side but the response was lost (timeout, dropped connection)
// - falling back blindly would duplicate the work. See issue #884.
func (o *Manager) tryNativeCopy(ctx context.Context, req *nativeCopyRequest) (string, bool, error) {
	copier, ok := req.destBackend.(s3be.BackendCopier)
	if !ok {
		return "", false, nil
	}
	cctx, ccancel := o.core.WithTimeout(ctx)
	defer ccancel()
	etag, err := copier.CopyObject(cctx, req.sourceKey, req.destKey, req.contentType, req.metadata)
	if err == nil {
		return o.finalizeNativeCopy(ctx, req, etag)
	}
	if errors.Is(err, s3be.ErrCopyNotSupported) {
		return "", false, nil
	}
	// Ambiguous failure: probe the destination via HEAD. If the
	// destination exists with the expected size, the copy completed
	// server-side; treat it as success and run the same post-success
	// path. Otherwise fall back to materialized copy.
	if recoveredETag, ok := o.probeDestAfterAmbiguousCopy(ctx, req, err); ok {
		return o.finalizeNativeCopy(ctx, req, recoveredETag)
	}
	o.log.WarnContext(ctx, "native copy failed, falling back to materialized copy",
		"source_key", req.sourceKey, "dest_key", req.destKey, "backend", req.destBackendName, "error", err)
	return "", false, nil
}

// finalizeNativeCopy runs the post-native-copy success steps shared by
// the happy path and the HEAD-probe recovery path: record the
// destination location, refresh accounting, mark the span as a native
// copy, emit completion observability, and invalidate caches. Returns
// (_, true, err) on RecordObjectOrCleanup failure - the bytes are
// already on the destination so the caller MUST NOT fall back.
func (o *Manager) finalizeNativeCopy(ctx context.Context, req *nativeCopyRequest, etag string) (string, bool, error) {
	const operation = "CopyObject"
	if err := o.coord.RecordObjectOrCleanup(ctx, req.span, req.destBackend, req.destKey, req.destBackendName, req.size, req.srcEnc); err != nil {
		return "", true, err
	}
	o.core.Acct().Operation(operation, req.destBackendName, req.start, nil)
	req.span.SetAttributes(telemetry.AttrNativeCopy.Bool(true))
	pobserve.CopyCompleted(ctx, req.span, req.sourceKey, req.destKey, req.destBackendName, req.destBackendName, req.size)
	o.invalidateObjectCaches(req.destKey)
	return etag, true, nil
}

// probeDestAfterAmbiguousCopy HEADs the destination after a non-
// capability native-copy error to disambiguate "copy succeeded server-
// side but the response was lost" from "copy actually failed." Returns
// (etag, true) when the destination exists and the size matches the
// expected source size; returns ("", false) otherwise so the caller
// falls back to materialized copy. A 404 on the HEAD is treated as a
// clean fallback signal; any other HEAD error is also a fallback but
// is logged as a warn so operators see the probe failure mode. See
// issue #884.
func (o *Manager) probeDestAfterAmbiguousCopy(ctx context.Context, req *nativeCopyRequest, origErr error) (string, bool) {
	hctx, hcancel := o.core.WithTimeout(ctx)
	defer hcancel()
	head, headErr := req.destBackend.HeadObject(hctx, req.destKey)
	if headErr != nil {
		if !s3be.IsNotFound(headErr) {
			o.log.WarnContext(ctx, "ambiguous native-copy HEAD probe failed",
				"source_key", req.sourceKey, "dest_key", req.destKey, "backend", req.destBackendName,
				"copy_error", origErr, "probe_error", headErr)
		}
		return "", false
	}
	if head.Size != req.size {
		o.log.WarnContext(ctx, "ambiguous native-copy destination size mismatch, falling back to materialized copy",
			"source_key", req.sourceKey, "dest_key", req.destKey, "backend", req.destBackendName,
			"expected_size", req.size, "observed_size", head.Size, "copy_error", origErr)
		return "", false
	}
	o.log.InfoContext(ctx, "ambiguous native-copy resolved via HEAD probe, destination already populated",
		"source_key", req.sourceKey, "dest_key", req.destKey, "backend", req.destBackendName,
		"size", head.Size, "copy_error", origErr)
	return head.ETag, true
}

// -------------------------------------------------------------------------
// DELETE
// -------------------------------------------------------------------------

// DeleteObject removes an object from the backend where it's stored.
func (o *Manager) DeleteObject(ctx context.Context, key string) error {
	const operation = "DeleteObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()

	// --- Delete all copies from store ---
	copies, err := o.stores.DeleteObject(ctx, key)
	if err != nil {
		if errors.Is(err, core.ErrObjectNotFound) {
			// Object not in our tracking - treat as success (idempotent delete)
			span.SetStatus(codes.Ok, "object not found - treating as success")
			return nil
		}
		return o.core.ClassifyWriteError(span, operation, err)
	}

	span.SetAttributes(attribute.Int("copies.deleted", len(copies)))

	// Drop the location cache entry up front so concurrent readers
	// during the backend fanout (which can take seconds) do not get
	// pointed at a backend that is in the middle of being deleted from.
	// The final invalidateObjectCaches below is a redundant no-op for
	// this key but keeps every mutation path ending with one helper.
	o.cache.Delete(key)

	// --- Delete from each backend that held a copy (fan out concurrently) ---
	workerpool.Run(ctx, len(copies), copies, func(ctx context.Context, cp core.DeletedCopy) {
		backend, ok := o.core.Backends()[cp.BackendName]
		if !ok {
			o.log.WarnContext(ctx, "backend not found for delete",
				"backend", cp.BackendName, "key", key)
			return
		}
		o.coord.DeleteOrEnqueue(ctx, backend, cp.BackendName, key, "delete_failed", cp.SizeBytes)
	})

	// --- Record metrics (use first copy's backend for primary) ---
	// Per-backend DELETE API-call accounting is owned by
	// DeleteOrEnqueue; recording it here too would double-count.
	if len(copies) > 0 {
		o.core.Acct().Operation(operation, copies[0].BackendName, start, nil)
	}

	pobserve.DeleteCompleted(ctx, span, key, len(copies))
	o.invalidateObjectCaches(key)
	return nil
}

// -------------------------------------------------------------------------
// BATCH DELETE
// -------------------------------------------------------------------------

// defaultBatchDeleteConcurrency caps how many per-key backend DELETE
// fanouts run at once inside DeleteObjects. Picked to absorb a typical
// S3 batch of 1000 keys without saturating any single backend's
// connection pool or burning API quota in a burst.
const defaultBatchDeleteConcurrency = 10

// DeleteObjectResult holds the outcome of a single key within a batch delete.
type DeleteObjectResult struct {
	Key string `json:"key,omitempty"`
	Err error  `json:"err,omitempty"`
}

// batchDeleteItem is one (key, backend) pair fanned out to the worker
// pool during DeleteObjects.
type batchDeleteItem struct {
	key       string
	backend   s3be.ObjectBackend
	beName    string
	sizeBytes int64
}

// DeleteObjects deletes multiple objects in a single request. Metadata
// removal happens in a single transaction via DeleteObjectsBatch; backend
// S3 deletes run concurrently with bounded parallelism to avoid
// overwhelming backends.
func (o *Manager) DeleteObjects(ctx context.Context, keys []string) []DeleteObjectResult {
	const operation = "DeleteObjects"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
		attribute.Int("s3o.batch_size", len(keys)),
	)
	defer span.End()

	results := make([]DeleteObjectResult, len(keys))
	for i, key := range keys {
		results[i].Key = key
	}

	copiesByKey, err := o.stores.DeleteObjectsBatch(ctx, keys)
	if err != nil {
		// Whole-tx failure: every key surfaces the error. The cache and
		// backend cleanup paths are skipped; nothing was changed.
		classified := o.core.ClassifyWriteError(span, operation, err)
		for i := range results {
			results[i].Err = classified
		}
		return results
	}

	// A key absent from copiesByKey was already gone (not-found is silent
	// success), so its cache entries are also stale and worth flushing.
	for _, key := range keys {
		o.invalidateObjectCaches(key)
	}

	deleteItems := o.flattenBatchDeletes(ctx, copiesByKey)
	workerpool.Run(ctx, defaultBatchDeleteConcurrency, deleteItems, func(ctx context.Context, item batchDeleteItem) {
		o.coord.DeleteOrEnqueue(ctx, item.backend, item.beName, item.key, "batch_delete_failed", item.sizeBytes)
	})

	successCount, errorCount := tallyDeleteResults(results)
	o.core.Acct().Operation(operation, "", start, nil)

	span.SetAttributes(
		attribute.Int("s3o.deleted_count", successCount),
		attribute.Int("s3o.error_count", errorCount),
	)
	pobserve.DeleteBatchCompleted(ctx, span, len(keys), successCount, errorCount)
	return results
}

// flattenBatchDeletes produces the worker-pool input slice from the
// DeleteObjectsBatch result. Skips copies whose backend is unknown
// (logged). Per-backend DELETE API-call accounting happens inside
// DeleteOrEnqueue when the item is consumed, so no tick is recorded
// here.
func (o *Manager) flattenBatchDeletes(ctx context.Context, copiesByKey map[string][]core.DeletedCopy) []batchDeleteItem {
	var items []batchDeleteItem
	for key, copies := range copiesByKey {
		for _, cp := range copies {
			backend, ok := o.core.Backends()[cp.BackendName]
			if !ok {
				o.log.WarnContext(ctx, "backend not found for batch delete",
					"backend", cp.BackendName, "key", key)
				continue
			}
			items = append(items, batchDeleteItem{
				key: key, backend: backend, beName: cp.BackendName, sizeBytes: cp.SizeBytes,
			})
		}
	}
	return items
}

// tallyDeleteResults counts how many entries in results carry an error
// versus succeeded. Returned for metrics and audit logging.
func tallyDeleteResults(results []DeleteObjectResult) (int, int) {
	var successCount, errorCount int
	for _, r := range results {
		if r.Err != nil {
			errorCount++
		} else {
			successCount++
		}
	}
	return successCount, errorCount
}

// materializedSource bundles the seekable reader handed to PutObject
// with the cleanup the caller must invoke once the upload settles. The
// returned source backend identifies which replica actually served the
// bytes so CopyObject can attribute usage correctly.
type materializedSource struct {
	body          io.ReadSeeker
	sourceBackend string
	cleanup       func()
}

// materializeCopySource reads the source object from the first
// reachable replica into a seekable buffer  -  in-memory for small
// objects, a self-unlinking tempfile for large ones  -  and returns
// it ready for handoff to PutObject. Failover iterates locations in
// order; backend-side errors (including the backend timeout firing
// per #882) are captured and a different replica is tried. When every
// replica fails the most recent underlying error is returned so
// callers can see why - the previous generic "failed to read source"
// string lost the DeadlineExceeded signal entirely. Per-replica GETs
// run under the backend timeout policy so a stalled source cannot
// exceed backend_timeout; a tighter caller deadline still wins.
func (o *Manager) materializeCopySource(
	ctx context.Context,
	sourceKey string,
	size int64,
	locations []core.ObjectLocation,
) (*materializedSource, error) {
	var lastErr error
	for i := range locations {
		ms, err := o.tryMaterializeFromLocation(ctx, sourceKey, size, locations[i].BackendName)
		if err != nil {
			lastErr = err
			continue
		}
		if ms != nil {
			return ms, nil
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("failed to read source from any copy")
}

// tryMaterializeFromLocation attempts to download sourceKey from one
// backend into a fresh seekable buffer. Returns (ms, nil) on success.
// A (nil, nil) return means the replica was skipped without a hard
// error (usage limits hit, backend not registered) - the caller moves
// to the next replica without capturing an error. A (nil, err) return
// is a real failure: the backend GET errored (including the
// backend-timeout context firing per #882) or the buffer-side
// materialization could not proceed. Errors are aggregated by the
// caller so the last underlying failure surfaces when no replica
// succeeds.
func (o *Manager) tryMaterializeFromLocation(
	ctx context.Context,
	sourceKey string,
	size int64,
	backendName string,
) (*materializedSource, error) {
	if !o.core.Usage().WithinLimits(backendName, 1, size, 0) {
		return nil, nil
	}
	be, ok := o.core.Backends()[backendName]
	if !ok {
		return nil, nil
	}

	// Wrap the source GET in the configured backend timeout so a
	// stalled replica cannot block the materialize step past
	// backend_timeout (#882). The same context covers the body
	// drain inside newMaterializedBody because rcancel only fires
	// on function return.
	rctx, rcancel := o.core.WithTimeout(ctx)
	defer rcancel()
	result, err := be.GetObject(rctx, sourceKey, "")
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()

	mb, err := newMaterializedBody(result.Body, size, nil)
	if err != nil {
		return nil, err
	}
	body, err := mb.Reader()
	if err != nil {
		mb.Cleanup()
		return nil, err
	}
	return &materializedSource{
		body:          body,
		sourceBackend: backendName,
		cleanup:       mb.Cleanup,
	}, nil
}


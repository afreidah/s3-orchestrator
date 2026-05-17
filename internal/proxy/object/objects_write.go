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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"

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

	bodyBytes, contentHash, err := o.bufferPutBody(span, body)
	if err != nil {
		return "", err
	}

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
			bodyBytes:   bodyBytes,
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
		o.core.Log().WarnContext(ctx, "PutObject: backend write failed, trying next",
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

// bufferPutBody copies body into memory so failover retries can replay
// the same plaintext, and computes the content hash when integrity
// verification is enabled.
func (o *Manager) bufferPutBody(span trace.Span, body io.Reader) ([]byte, string, error) {
	var buf bytes.Buffer
	if _, err := bufpool.Copy(&buf, body); err != nil {
		observe.RecordSpanError(span, err)
		return nil, "", fmt.Errorf("buffer request body: %w", err)
	}
	bodyBytes := buf.Bytes()
	var contentHash string
	if icfg := o.integrityCfg.Load(); icfg != nil && icfg.Enabled {
		contentHash = HashBody(bodyBytes)
	}
	return bodyBytes, contentHash, nil
}

// putAttemptRequest bundles the per-attempt arguments to
// attemptPutOnBackend so the helper signature stays under the
// parameter-count limit.
type putAttemptRequest struct {
	operation   string
	key         string
	bodyBytes   []byte
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

	uploadBody, uploadSize, enc, err := o.buildPutPayload(ctx, req.bodyBytes, req.size, req.contentHash, req.dekState)
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
		o.core.Usage().Record(backendName, 1, 0, 0)
		// Leave the pending row for the reaper. A backend PUT error does
		// not reliably mean the bytes are absent: the response could have
		// been lost mid-flight, so the reaper HEADs the backend on its
		// next tick and either promotes or drops the intent.
		return putAttemptResult{backend: backendName, putErr: err}
	}

	if err := o.coord.RecordObjectAndPromoteIntent(ctx, span, req.key, backendName, uploadSize, enc, intentID); err != nil {
		return putAttemptResult{backend: backendName, fatalErr: err}
	}
	o.cache.Delete(req.key)
	return putAttemptResult{backend: backendName, etag: etag}
}

// buildPutPayload prepares the upload body and EncryptionMeta for a
// single attempt. Encryption layering, when enabled, runs through
// encryptForPut so the wrapped DEK is reused across failover retries.
func (o *Manager) buildPutPayload(
	ctx context.Context,
	bodyBytes []byte,
	size int64,
	contentHash string,
	dekState *putEncryptState,
) (io.Reader, int64, *core.EncryptionMeta, error) {
	if o.encryptor != nil {
		uploadBody, uploadSize, enc, err := encryptForPut(ctx, o.encryptor, bodyBytes, size, dekState)
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
	return bytes.NewReader(bodyBytes), size, enc, nil
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
	o.core.RecordOperation(req.operation, req.backendName, req.start, nil)
	o.core.Usage().Record(req.backendName, 1, 0, req.size)
	if len(req.failedBackends) > 0 {
		for _, fb := range req.failedBackends {
			telemetry.WriteFailoverTotal.WithLabelValues(req.operation, fb, req.backendName).Inc()
		}
		span.SetAttributes(telemetry.AttrWriteFailover.Bool(true))
		span.SetAttributes(telemetry.AttrFailoverAttempts.Int(len(req.failedBackends)))
	}
	audit.Log(ctx, "storage.PutObject",
		slog.String("key", req.key),
		slog.String("backend", req.backendName),
		slog.Int64("size", req.size),
	)
	if event.Emit != nil {
		bucket, userKey := internalkey.Split(req.key)
		event.Emit(event.Event{
			Type:    event.ObjectCreatedPut,
			Subject: userKey,
			Data: map[string]any{
				"bucket":     bucket,
				"key":        userKey,
				"backend":    req.backendName,
				"size":       req.size,
				"request_id": audit.RequestID(ctx),
			},
		})
	}
	o.invalidateCache(req.key)
	span.SetStatus(codes.Ok, "")
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
// objects, a self-unlinking tempfile above copyMaterializeMemThreshold
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

	src, err := o.materializeCopySource(ctx, sourceKey, size, locations)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}
	defer src.cleanup()

	// Seekable body keeps the SDK on UNSIGNED-PAYLOAD signing so
	// Content-Length survives to the backend.
	etag, err := destBackend.PutObject(ctx, destKey, src.body, size, contentType, metadata)
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

	// --- Invalidate location cache ---
	o.cache.Delete(destKey)

	o.core.RecordOperation(operation, destBackendName, start, nil)
	srcName := src.sourceBackend
	o.core.Usage().Record(srcName, 1, size, 0)         // source: Get + egress
	o.core.Usage().Record(destBackendName, 1, 0, size) // dest: Put + ingress

	audit.Log(ctx, "storage.CopyObject",
		slog.String("source_key", sourceKey),
		slog.String("dest_key", destKey),
		slog.String("source_backend", srcName),
		slog.String("dest_backend", destBackendName),
		slog.Int64("size", size),
	)
	if event.Emit != nil {
		bucket, userKey := internalkey.Split(destKey)
		event.Emit(event.Event{
			Type:    event.ObjectCreatedCopy,
			Subject: userKey,
			Data: map[string]any{
				"bucket":     bucket,
				"key":        userKey,
				"source_key": sourceKey,
				"backend":    destBackendName,
				"size":       size,
				"request_id": audit.RequestID(ctx),
			},
		})
	}
	o.invalidateCache(destKey)

	span.SetStatus(codes.Ok, "")
	return etag, nil
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

	// --- Invalidate location cache ---
	o.cache.Delete(key)

	// --- Delete from each backend that held a copy (fan out concurrently) ---
	workerpool.Run(ctx, len(copies), copies, func(ctx context.Context, cp core.DeletedCopy) {
		backend, ok := o.core.Backends()[cp.BackendName]
		if !ok {
			o.core.Log().WarnContext(ctx, "backend not found for delete",
				"backend", cp.BackendName, "key", key)
			return
		}
		o.coord.DeleteOrEnqueue(ctx, backend, cp.BackendName, key, "delete_failed", cp.SizeBytes)
	})

	// --- Record metrics (use first copy's backend for primary) ---
	if len(copies) > 0 {
		o.core.RecordOperation(operation, copies[0].BackendName, start, nil)
	}
	for _, c := range copies {
		o.core.Usage().Record(c.BackendName, 1, 0, 0)
	}

	audit.Log(ctx, "storage.DeleteObject",
		slog.String("key", key),
		slog.Int("copies_deleted", len(copies)),
	)
	if event.Emit != nil {
		bucket, userKey := internalkey.Split(key)
		event.Emit(event.Event{
			Type:    event.ObjectRemovedDelete,
			Subject: userKey,
			Data: map[string]any{
				"bucket":         bucket,
				"key":            userKey,
				"copies_deleted": len(copies),
				"request_id":     audit.RequestID(ctx),
			},
		})
	}
	o.invalidateCache(key)

	span.SetStatus(codes.Ok, "")
	return nil
}

// -------------------------------------------------------------------------
// BATCH DELETE
// -------------------------------------------------------------------------

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
		o.cache.Delete(key)
		o.invalidateCache(key)
	}

	deleteItems := o.flattenBatchDeletes(ctx, copiesByKey)
	workerpool.Run(ctx, 10, deleteItems, func(ctx context.Context, item batchDeleteItem) {
		o.coord.DeleteOrEnqueue(ctx, item.backend, item.beName, item.key, "batch_delete_failed", item.sizeBytes)
	})

	successCount, errorCount := tallyDeleteResults(results)
	o.core.RecordOperation(operation, "", start, nil)

	audit.Log(ctx, "storage.DeleteObjects",
		slog.Int("total_keys", len(keys)),
		slog.Int("deleted", successCount),
		slog.Int("errors", errorCount),
	)
	if event.Emit != nil {
		event.Emit(event.Event{
			Type: event.ObjectRemovedDeleteBatch,
			Data: map[string]any{
				"total_keys": len(keys),
				"deleted":    successCount,
				"errors":     errorCount,
				"request_id": audit.RequestID(ctx),
			},
		})
	}

	span.SetAttributes(
		attribute.Int("s3o.deleted_count", successCount),
		attribute.Int("s3o.error_count", errorCount),
	)
	span.SetStatus(codes.Ok, "")

	return results
}

// flattenBatchDeletes produces the worker-pool input slice from the
// DeleteObjectsBatch result. Skips copies whose backend is unknown
// (logged) and records a usage tick per emitted copy.
func (o *Manager) flattenBatchDeletes(ctx context.Context, copiesByKey map[string][]core.DeletedCopy) []batchDeleteItem {
	var items []batchDeleteItem
	for key, copies := range copiesByKey {
		for _, cp := range copies {
			backend, ok := o.core.Backends()[cp.BackendName]
			if !ok {
				o.core.Log().WarnContext(ctx, "backend not found for batch delete",
					"backend", cp.BackendName, "key", key)
				continue
			}
			items = append(items, batchDeleteItem{
				key: key, backend: backend, beName: cp.BackendName, sizeBytes: cp.SizeBytes,
			})
			o.core.Usage().Record(cp.BackendName, 1, 0, 0)
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

// copyMaterializeMemThreshold is the largest source object size that
// materializeCopySource keeps in memory. Above this, the helper spills
// to a tempfile. Mirrors the AWS SDK's own internal heuristic for the
// PUT signing path; not a config knob because copy-vs-tempfile is an
// implementation detail, not an operator concern.
const copyMaterializeMemThreshold = 32 * 1024 * 1024

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
// order and resets the buffer between attempts so a partial read from
// one replica does not contaminate the next try. Returns an error only
// when every replica fails or buffering itself errors. The supplied
// ctx flows unmodified into the backend GetObject calls; the caller
// owns timeout and cancellation policy.
func (o *Manager) materializeCopySource(
	ctx context.Context,
	sourceKey string,
	size int64,
	locations []core.ObjectLocation,
) (*materializedSource, error) {
	for i := range locations {
		ms, ok, err := o.tryMaterializeFromLocation(ctx, sourceKey, size, locations[i].BackendName)
		if err != nil {
			return nil, err
		}
		if ok {
			return ms, nil
		}
	}
	return nil, fmt.Errorf("failed to read source from any copy")
}

// tryMaterializeFromLocation attempts to download sourceKey from one
// backend into a fresh seekable buffer. Returns ok=true when the read
// completed and the buffer is ready for PutObject. ok=false means the
// caller should move on to the next replica; err is reserved for
// buffer-side failures (out of memory, tempfile creation) that cannot
// be retried by switching replicas.
func (o *Manager) tryMaterializeFromLocation(
	ctx context.Context,
	sourceKey string,
	size int64,
	backendName string,
) (*materializedSource, bool, error) {
	if !o.core.Usage().WithinLimits(backendName, 1, size, 0) {
		return nil, false, nil
	}
	be, ok := o.core.Backends()[backendName]
	if !ok {
		return nil, false, nil
	}

	result, err := be.GetObject(ctx, sourceKey, "")
	if err != nil {
		return nil, false, nil
	}
	defer result.Body.Close()

	sink, cleanup, err := newCopyMaterializeSink(size)
	if err != nil {
		return nil, false, err
	}
	if _, err := bufpool.Copy(sink.writer(), result.Body); err != nil {
		cleanup()
		return nil, false, nil
	}
	body, err := sink.seekableBody()
	if err != nil {
		cleanup()
		return nil, false, err
	}
	return &materializedSource{
		body:          body,
		sourceBackend: backendName,
		cleanup:       cleanup,
	}, true, nil
}

// copyMaterializeSink hides whether the materialized body lives in
// memory or on disk so tryMaterializeFromLocation can write through a
// uniform interface and only branch on which seekable reader to
// return.
type copyMaterializeSink struct {
	buf  *bytes.Buffer
	file *os.File
}

// newCopyMaterializeSink picks the in-memory or tempfile sink based on
// the known object size. Returns a cleanup that the caller must invoke
// once the upload settles, including on materialization error.
func newCopyMaterializeSink(size int64) (*copyMaterializeSink, func(), error) {
	if size <= copyMaterializeMemThreshold {
		noopCleanup := func() {
			// In-memory sink owns no resource; the buffer is
			// reclaimed by the GC when the materializedSource goes
			// out of scope. Cleanup is a no-op so the caller can
			// always defer it without branching on sink type.
		}
		return &copyMaterializeSink{buf: &bytes.Buffer{}}, noopCleanup, nil
	}
	f, err := os.CreateTemp("", "s3o-copy-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create copy tempfile: %w", err)
	}
	// Unlink immediately so the file disappears on Close or process
	// exit; cleanup() only needs to close the fd. Removes the
	// possibility of leaking a tempfile if the process panics
	// mid-copy.
	_ = os.Remove(f.Name()) //nolint:gosec // G703: path comes from os.CreateTemp, not user input
	return &copyMaterializeSink{file: f}, func() { _ = f.Close() }, nil
}

// writer returns the io.Writer the source body streams into.
func (s *copyMaterializeSink) writer() io.Writer {
	if s.file != nil {
		return s.file
	}
	return s.buf
}

// seekableBody returns the io.ReadSeeker handed to PutObject. For the
// tempfile sink this rewinds the file so PutObject reads from offset 0.
func (s *copyMaterializeSink) seekableBody() (io.ReadSeeker, error) {
	if s.file != nil {
		if _, err := s.file.Seek(0, io.SeekStart); err != nil { //nolint:gosec // G703: file path comes from os.CreateTemp("", ...), not user input
			return nil, fmt.Errorf("rewind copy tempfile: %w", err)
		}
		return s.file, nil
	}
	return bytes.NewReader(s.buf.Bytes()), nil
}

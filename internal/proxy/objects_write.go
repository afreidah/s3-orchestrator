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

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// -------------------------------------------------------------------------
// OBJECT CRUD
// -------------------------------------------------------------------------

// PutObject uploads an object to the first backend with available quota.
// If the upload fails, it retries on remaining eligible backends before
// returning an error to the caller (write failover).
func (o *ObjectManager) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error) {
	const operation = "PutObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		telemetry.AttrObjectKey.String(key),
		telemetry.AttrObjectSize.Int64(size),
	)
	defer span.End()

	// --- Filter backends within usage limits, exclude draining and unhealthy ---
	eligible := o.eligibleForWrite(1, 0, size)
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "write").Inc()
		span.SetStatus(codes.Error, "usage limits exceeded on all backends")
		return "", store.ErrInsufficientStorage
	}

	// --- Buffer body for retry ---
	// io.Reader is single-use; buffer the plaintext so we can replay on failover.
	var buf bytes.Buffer
	if _, err := bufpool.Copy(&buf, body); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("buffer request body: %w", err)
	}
	bodyBytes := buf.Bytes()

	// --- Compute content hash if integrity is enabled ---
	var contentHash string
	if icfg := o.integrityCfg(); icfg != nil && icfg.Enabled {
		contentHash = HashBody(bodyBytes)
	}

	// --- Try eligible backends with failover ---
	var failedBackends []string
	var lastErr error
	for len(eligible) > 0 {
		backendName, err := o.selectBackendForWrite(ctx, size, eligible)
		if err != nil {
			return "", o.classifyWriteError(span, operation, err)
		}

		span.SetAttributes(telemetry.AttrBackendName.String(backendName))

		be, err := o.getBackend(backendName)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return "", err
		}

		// --- Encrypt if enabled (re-encrypt per attempt — unique nonce each time) ---
		var enc *store.EncryptionMeta
		uploadBody := io.Reader(bytes.NewReader(bodyBytes))
		uploadSize := size
		if o.encryptor != nil {
			encResult, err := o.encryptor.Encrypt(ctx, bytes.NewReader(bodyBytes), size)
			if err != nil {
				telemetry.EncryptionErrorsTotal.WithLabelValues("encrypt", "encrypt_failed").Inc()
				span.SetStatus(codes.Error, err.Error())
				return "", fmt.Errorf("encrypt: %w", err)
			}
			telemetry.EncryptionOpsTotal.WithLabelValues("encrypt").Inc()
			uploadBody = encResult.Body
			uploadSize = encResult.CiphertextSize
			enc = &store.EncryptionMeta{
				Encrypted:     true,
				EncryptionKey: encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK),
				KeyID:         encResult.KeyID,
				PlaintextSize: size,
				ContentHash:   contentHash,
			}
		} else if contentHash != "" {
			enc = &store.EncryptionMeta{ContentHash: contentHash}
		}

		// --- Upload to backend ---
		bctx, bcancel := o.withTimeout(ctx)
		etag, err := be.PutObject(bctx, key, uploadBody, uploadSize, contentType, metadata)
		bcancel()
		if err != nil {
			o.usage.Record(backendName, 1, 0, 0) // API call was made even on failure
			lastErr = err
			failedBackends = append(failedBackends, backendName)

			// Remove failed backend from eligible list and retry
			remaining := make([]string, 0, len(eligible)-1)
			for _, name := range eligible {
				if name != backendName {
					remaining = append(remaining, name)
				}
			}
			eligible = remaining

			slog.WarnContext(ctx, "PutObject: backend write failed, trying next",
				"key", key, "failed_backend", backendName, "error", err,
				"remaining_backends", len(eligible))
			continue
		}

		// --- Record object location and update quota ---
		if err := o.recordObjectOrCleanup(ctx, span, be, key, backendName, uploadSize, enc); err != nil {
			return "", err
		}

		// --- Invalidate location cache ---
		o.cache.Delete(key)

		// --- Record metrics ---
		o.recordOperation(operation, backendName, start, nil)
		o.usage.Record(backendName, 1, 0, size)

		if len(failedBackends) > 0 {
			for _, fb := range failedBackends {
				telemetry.WriteFailoverTotal.WithLabelValues(operation, fb, backendName).Inc()
			}
			span.SetAttributes(telemetry.AttrWriteFailover.Bool(true))
			span.SetAttributes(telemetry.AttrFailoverAttempts.Int(len(failedBackends)))
		}

		audit.Log(ctx, "storage.PutObject",
			slog.String("key", key),
			slog.String("backend", backendName),
			slog.Int64("size", size),
		)
		if event.Emit != nil {
			bucket, userKey := splitInternalKey(key)
			event.Emit(event.Event{
				Type:    event.ObjectCreatedPut,
				Subject: userKey,
				Data: map[string]any{
					"bucket":     bucket,
					"key":        userKey,
					"backend":    backendName,
					"size":       size,
					"request_id": audit.RequestID(ctx),
				},
			})
		}
		o.invalidateCache(key)

		span.SetStatus(codes.Ok, "")
		return etag, nil
	}

	// All eligible backends exhausted
	span.SetStatus(codes.Error, lastErr.Error())
	span.RecordError(lastErr)
	return "", lastErr
}

// -------------------------------------------------------------------------
// COPY
// -------------------------------------------------------------------------

// CopyObject copies an object from sourceKey to destKey. Streams the source
// through a pipe to avoid buffering the entire object. Supports cross-backend
// copies and read failover from replicas.
func (o *ObjectManager) CopyObject(ctx context.Context, sourceKey, destKey string) (string, error) {
	const operation = "CopyObject"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		attribute.String("s3o.source_key", sourceKey),
		attribute.String("s3o.dest_key", destKey),
	)
	defer span.End()

	// --- Find all source locations (for failover) ---
	locations, err := o.store.GetAllObjectLocations(ctx, sourceKey)
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			span.SetStatus(codes.Error, "source object not found")
			return "", err
		}
		return "", o.classifyWriteError(span, operation, err)
	}

	// --- Get source metadata (try each copy, skip over-limit backends) ---
	var size int64
	var contentType string
	var metadata map[string]string
	var srcFound bool
	var srcEnc *store.EncryptionMeta
	for i := range locations {
		if !o.usage.WithinLimits(locations[i].BackendName, 1, 0, 0) {
			continue
		}
		be, ok := o.backends[locations[i].BackendName]
		if !ok {
			continue
		}
		bctx, bcancel := o.withTimeout(ctx)
		headResult, err := be.HeadObject(bctx, sourceKey)
		bcancel()
		if err != nil {
			continue
		}
		size = headResult.Size
		contentType = headResult.ContentType
		metadata = headResult.Metadata
		srcFound = true
		if locations[i].Encrypted {
			srcEnc = &store.EncryptionMeta{
				Encrypted:     true,
				EncryptionKey: locations[i].EncryptionKey,
				KeyID:         locations[i].KeyID,
				PlaintextSize: locations[i].PlaintextSize,
			}
		}
		break
	}
	if !srcFound {
		err := fmt.Errorf("failed to head source object from any copy")
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	span.SetAttributes(telemetry.AttrObjectSize.Int64(size))

	// --- Find destination backend with available quota ---
	destBackendName, err := o.selectWriteTarget(ctx, span, operation, size)
	if err != nil {
		return "", err
	}

	destBackend, err := o.getBackend(destBackendName)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	// --- Stream source to destination via pipe (with failover) ---
	pr, pw := io.Pipe()
	srcBackendCh := make(chan string, 1)

	go func() {
		bw := bufpool.GetWriter(pw)
		defer func() {
			bufpool.PutWriter(bw)
			_ = pw.Close()
		}()
		for li := range locations {
			if !o.usage.WithinLimits(locations[li].BackendName, 1, size, 0) {
				continue
			}
			srcBackend, ok := o.backends[locations[li].BackendName]
			if !ok {
				continue
			}
			bctx, bcancel := o.withTimeout(ctx)
			result, err := srcBackend.GetObject(bctx, sourceKey, "")
			if err != nil {
				bcancel()
				continue
			}
			srcBackendCh <- locations[li].BackendName
			_, copyErr := bufpool.Copy(bw, result.Body)
			_ = result.Body.Close()
			bcancel()
			if copyErr != nil {
				pw.CloseWithError(fmt.Errorf("failed to stream source: %w", copyErr))
				return
			}
			if flushErr := bw.Flush(); flushErr != nil {
				pw.CloseWithError(fmt.Errorf("failed to flush source stream: %w", flushErr))
				return
			}
			return
		}
		close(srcBackendCh)
		pw.CloseWithError(fmt.Errorf("failed to read source from any copy"))
	}()

	// Streamed from pipe; deadline governed by the caller's context.
	etag, err := destBackend.PutObject(ctx, destKey, pr, size, contentType, metadata)
	pr.Close() // unblock goroutine if PutObject returned without draining the pipe
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return "", fmt.Errorf("failed to write destination: %w", err)
	}

	// --- Record destination location and update quota ---
	// Preserve encryption metadata: ciphertext is copied as-is so the
	// destination keeps the same wrapped DEK and key ID.
	if err := o.recordObjectOrCleanup(ctx, span, destBackend, destKey, destBackendName, size, srcEnc); err != nil {
		return "", err
	}

	// --- Invalidate location cache ---
	o.cache.Delete(destKey)

	o.recordOperation(operation, destBackendName, start, nil)
	var srcName string
	if sn, ok := <-srcBackendCh; ok {
		srcName = sn
		o.usage.Record(srcName, 1, size, 0) // source: Get + egress
	}
	o.usage.Record(destBackendName, 1, 0, size) // dest: Put + ingress

	audit.Log(ctx, "storage.CopyObject",
		slog.String("source_key", sourceKey),
		slog.String("dest_key", destKey),
		slog.String("source_backend", srcName),
		slog.String("dest_backend", destBackendName),
		slog.Int64("size", size),
	)
	if event.Emit != nil {
		bucket, userKey := splitInternalKey(destKey)
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
func (o *ObjectManager) DeleteObject(ctx context.Context, key string) error {
	const operation = "DeleteObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()

	// --- Delete all copies from store ---
	copies, err := o.store.DeleteObject(ctx, key)
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			// Object not in our tracking - treat as success (idempotent delete)
			span.SetStatus(codes.Ok, "object not found - treating as success")
			return nil
		}
		return o.classifyWriteError(span, operation, err)
	}

	span.SetAttributes(attribute.Int("copies.deleted", len(copies)))

	// --- Invalidate location cache ---
	o.cache.Delete(key)

	// --- Delete from each backend that held a copy (fan out concurrently) ---
	workerpool.Run(ctx, len(copies), copies, func(ctx context.Context, cp store.DeletedCopy) {
		backend, ok := o.backends[cp.BackendName]
		if !ok {
			slog.WarnContext(ctx, "Backend not found for delete",
				"backend", cp.BackendName, "key", key)
			return
		}
		o.deleteOrEnqueue(ctx, backend, cp.BackendName, key, "delete_failed", cp.SizeBytes)
	})

	// --- Record metrics (use first copy's backend for primary) ---
	if len(copies) > 0 {
		o.recordOperation(operation, copies[0].BackendName, start, nil)
	}
	for _, c := range copies {
		o.usage.Record(c.BackendName, 1, 0, 0)
	}

	audit.Log(ctx, "storage.DeleteObject",
		slog.String("key", key),
		slog.Int("copies_deleted", len(copies)),
	)
	if event.Emit != nil {
		bucket, userKey := splitInternalKey(key)
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
	Key string
	Err error
}

// DeleteObjects deletes multiple objects in a single request. Metadata removal
// happens sequentially (each key is its own transaction via the existing
// store.DeleteObject path), while backend S3 deletes run concurrently with
// bounded parallelism to avoid overwhelming backends.
func (o *ObjectManager) DeleteObjects(ctx context.Context, keys []string) []DeleteObjectResult {
	const operation = "DeleteObjects"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		attribute.Int("s3o.batch_size", len(keys)),
	)
	defer span.End()

	results := make([]DeleteObjectResult, len(keys))

	// Remove each key from the DB and collect backend copies that need
	// physical deletion afterwards.
	type pendingBackendDelete struct {
		key    string
		copies []store.DeletedCopy
	}
	var pending []pendingBackendDelete

	for i, key := range keys {
		results[i].Key = key

		copies, err := o.store.DeleteObject(ctx, key)
		if err != nil {
			if errors.Is(err, store.ErrObjectNotFound) {
				continue // not-found treated as success
			}
			results[i].Err = o.classifyWriteError(span, operation, err)
			continue
		}

		o.cache.Delete(key)
		o.invalidateCache(key)

		if len(copies) > 0 {
			pending = append(pending, pendingBackendDelete{key: key, copies: copies})
		}
	}

	// Flatten pending deletes into a single work slice for the pool
	type batchDeleteItem struct {
		key       string
		backend   s3be.ObjectBackend
		beName    string
		sizeBytes int64
	}
	var deleteItems []batchDeleteItem
	for _, pd := range pending {
		for _, cp := range pd.copies {
			backend, ok := o.backends[cp.BackendName]
			if !ok {
				slog.WarnContext(ctx, "Backend not found for batch delete",
					"backend", cp.BackendName, "key", pd.key)
				continue
			}
			deleteItems = append(deleteItems, batchDeleteItem{
				key: pd.key, backend: backend, beName: cp.BackendName, sizeBytes: cp.SizeBytes,
			})
		}
		for _, cp := range pd.copies {
			o.usage.Record(cp.BackendName, 1, 0, 0)
		}
	}

	// Delete from backends concurrently, capped at 10 in-flight calls.
	var mu sync.Mutex
	var backendErrors []error

	workerpool.Run(ctx, 10, deleteItems, func(ctx context.Context, item batchDeleteItem) {
		err := o.deleteWithTimeout(ctx, item.backend, item.key)
		if err != nil {
			slog.WarnContext(ctx, "Failed to delete object from backend (batch)",
				"backend", item.beName, "key", item.key, "error", err)
			o.enqueueCleanup(ctx, item.beName, item.key, "batch_delete_failed", item.sizeBytes)
			mu.Lock()
			backendErrors = append(backendErrors, err)
			mu.Unlock()
		}
	})

	// Record backend errors on the span after all goroutines have finished.
	for _, err := range backendErrors {
		span.RecordError(err)
	}

	// Tally outcomes for metrics and audit
	var successCount, errorCount int
	for _, r := range results {
		if r.Err != nil {
			errorCount++
		} else {
			successCount++
		}
	}

	o.recordOperation(operation, "", start, nil)

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

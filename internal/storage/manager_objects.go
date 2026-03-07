// -------------------------------------------------------------------------------
// Object Operations - PUT, GET, HEAD, DELETE, COPY, DeleteObjects
//
// Author: Alex Freidah
//
// Object-level CRUD operations on the BackendManager. Handles backend selection
// via routing strategy, read failover across replicas, broadcast reads during
// degraded mode, and usage limit enforcement on reads and writes. DeleteObjects
// provides batch deletion with concurrent backend I/O.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// wrapReader returns an io.ReadCloser that reads from r but closes c.
// Used to replace io.NopCloser when the decrypt reader wraps a backend
// response body — Close must still reach the original body so the
// underlying HTTP connection is released.
func wrapReader(r io.Reader, c io.Closer) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{Reader: r, Closer: c}
}

// -------------------------------------------------------------------------
// OBJECT CRUD
// -------------------------------------------------------------------------

// PutObject uploads an object to the first backend with available quota.
func (m *BackendManager) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error) {
	const operation = "PutObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		telemetry.AttrObjectKey.String(key),
		telemetry.AttrObjectSize.Int64(size),
	)
	defer span.End()

	// --- Filter backends within usage limits and exclude draining ---
	eligible := m.excludeDraining(m.usage.BackendsWithinLimits(m.order, 1, 0, size))
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "write").Inc()
		span.SetStatus(codes.Error, "usage limits exceeded on all backends")
		return "", ErrInsufficientStorage
	}

	// --- Find backend with available quota ---
	backendName, err := m.selectBackendForWrite(ctx, size, eligible)
	if err != nil {
		return "", m.classifyWriteError(span, operation, err)
	}

	span.SetAttributes(telemetry.AttrBackendName.String(backendName))

	// --- Get the backend ---
	backend, err := m.getBackend(backendName)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	// --- Encrypt if enabled ---
	var enc *EncryptionMeta
	uploadBody := body
	uploadSize := size
	if m.encryptor != nil {
		encResult, err := m.encryptor.Encrypt(ctx, body, size)
		if err != nil {
			telemetry.EncryptionErrorsTotal.WithLabelValues("encrypt", "encrypt_failed").Inc()
			span.SetStatus(codes.Error, err.Error())
			return "", fmt.Errorf("encrypt: %w", err)
		}
		telemetry.EncryptionOpsTotal.WithLabelValues("encrypt").Inc()
		uploadBody = encResult.Body
		uploadSize = encResult.CiphertextSize
		enc = &EncryptionMeta{
			Encrypted:     true,
			EncryptionKey: encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK),
			KeyID:         encResult.KeyID,
			PlaintextSize: size,
		}
	}

	// --- Upload to backend ---
	bctx, bcancel := m.withTimeout(ctx)
	defer bcancel()
	etag, err := backend.PutObject(bctx, key, uploadBody, uploadSize, contentType, metadata)
	if err != nil {
		m.usage.Record(backendName, 1, 0, 0) // API call was made even on failure
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return "", err
	}

	// --- Record object location and update quota ---
	if err := m.recordObjectOrCleanup(ctx, span, backend, key, backendName, uploadSize, enc); err != nil {
		return "", err
	}

	// --- Invalidate location cache ---
	m.cache.Delete(key)

	// --- Record metrics ---
	m.recordOperation(operation, backendName, start, nil)
	m.usage.Record(backendName, 1, 0, size)

	audit.Log(ctx, "storage.PutObject",
		slog.String("key", key),
		slog.String("backend", backendName),
		slog.Int64("size", size),
	)

	span.SetStatus(codes.Ok, "")
	return etag, nil
}

// -------------------------------------------------------------------------
// READ FAILOVER
// -------------------------------------------------------------------------

// errUsageLimitSkip is an internal sentinel indicating a backend was skipped
// because it exceeded its monthly usage limits. Not exposed to callers.
var errUsageLimitSkip = errors.New("backend skipped: usage limits exceeded")

// withReadFailover looks up all copies of an object and tries each backend in
// order until one succeeds. The tryBackend callback should attempt the operation
// and return the object size (for span attributes) or an error to try the next copy.
// Returns the name of the backend that succeeded alongside any error.
func (m *BackendManager) withReadFailover(ctx context.Context, operation, key string, tryBackend func(ctx context.Context, backendName string, backend ObjectBackend) (int64, error)) (string, error) {
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()

	locations, err := m.store.GetAllObjectLocations(ctx, key)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			span.SetStatus(codes.Error, "object not found")
			return "", err
		}
		if errors.Is(err, ErrDBUnavailable) {
			return m.broadcastRead(ctx, operation, key, start, span, tryBackend)
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return "", fmt.Errorf("failed to find object location: %w", err)
	}

	var lastErr error
	var limitSkips int
	for i, loc := range locations {
		span.SetAttributes(telemetry.AttrBackendName.String(loc.BackendName))

		backend, ok := m.backends[loc.BackendName]
		if !ok {
			lastErr = fmt.Errorf("backend %s not found", loc.BackendName)
			continue
		}

		bctx, bcancel := m.withTimeout(ctx)
		size, err := tryBackend(bctx, loc.BackendName, backend)
		if err != nil {
			bcancel()
			lastErr = err
			if errors.Is(err, errUsageLimitSkip) {
				limitSkips++
			}
			if i < len(locations)-1 {
				slog.Warn(operation+": copy failed, trying next",
					"key", key, "failed_backend", loc.BackendName, "error", err)
			}
			continue
		}

		m.recordOperation(operation, loc.BackendName, start, nil)
		if i > 0 {
			span.SetAttributes(attribute.Bool("s3proxy.failover", true))
		}
		span.SetAttributes(telemetry.AttrObjectSize.Int64(size))
		span.SetStatus(codes.Ok, "")
		return loc.BackendName, nil
	}

	// All copies were on over-limit backends — return the usage limit error
	if limitSkips > 0 && limitSkips == len(locations) {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "read").Inc()
		span.SetStatus(codes.Error, "all copies over usage limit")
		return "", ErrUsageLimitExceeded
	}

	span.SetStatus(codes.Error, lastErr.Error())
	span.RecordError(lastErr)
	return "", lastErr
}

// broadcastRead tries all backends when the DB is unavailable. Checks the
// location cache first for a known-good backend, then dispatches to either
// parallel or sequential broadcast based on configuration.
func (m *BackendManager) broadcastRead(ctx context.Context, operation, key string, start time.Time, span trace.Span, tryBackend func(ctx context.Context, backendName string, backend ObjectBackend) (int64, error)) (string, error) {
	span.SetAttributes(attribute.Bool("s3proxy.degraded_mode", true))
	telemetry.DegradedReadsTotal.WithLabelValues(operation).Inc()

	// --- Check location cache first ---
	if cachedBackend, ok := m.cache.Get(key); ok {
		if backend, exists := m.backends[cachedBackend]; exists {
			bctx, bcancel := m.withTimeout(ctx)
			size, err := tryBackend(bctx, cachedBackend, backend)
			if err == nil {
				m.recordOperation(operation, cachedBackend, start, nil)
				span.SetAttributes(attribute.Bool("s3proxy.cache_hit", true))
				span.SetAttributes(telemetry.AttrObjectSize.Int64(size))
				span.SetStatus(codes.Ok, "")
				telemetry.DegradedCacheHitsTotal.Inc()
				return cachedBackend, nil
			}
			bcancel()
			// Cache hit but backend failed — fall through to broadcast
		}
	}

	if m.parallelBroadcast {
		return m.parallelBroadcastRead(ctx, operation, key, start, span, tryBackend)
	}
	return m.sequentialBroadcastRead(ctx, operation, key, start, span, tryBackend)
}

// sequentialBroadcastRead tries each backend in order until one succeeds.
func (m *BackendManager) sequentialBroadcastRead(ctx context.Context, operation, key string, start time.Time, span trace.Span, tryBackend func(ctx context.Context, backendName string, backend ObjectBackend) (int64, error)) (string, error) {
	var lastErr error
	for _, name := range m.order {
		backend, ok := m.backends[name]
		if !ok {
			continue
		}
		bctx, bcancel := m.withTimeout(ctx)
		size, err := tryBackend(bctx, name, backend)
		if err != nil {
			bcancel()
			lastErr = err
			continue
		}

		// Success — cache the result for future degraded reads
		m.cache.Set(key, name)
		m.recordOperation(operation, name, start, nil)
		span.SetAttributes(telemetry.AttrBackendName.String(name))
		span.SetAttributes(telemetry.AttrObjectSize.Int64(size))
		span.SetStatus(codes.Ok, "")
		return name, nil
	}

	// All backends failed — return the actual error so the server can
	// distinguish "object not found" (404) from "backends unreachable" (502).
	if lastErr != nil {
		span.SetStatus(codes.Error, lastErr.Error())
		span.RecordError(lastErr)
		return "", fmt.Errorf("all backends failed during degraded read: %w", lastErr)
	}

	span.SetStatus(codes.Error, "no backends available")
	return "", ErrObjectNotFound
}

// parallelBroadcastRead fans out to all backends concurrently and returns
// the first successful result. Remaining goroutines are cancelled via context.
func (m *BackendManager) parallelBroadcastRead(ctx context.Context, operation, key string, start time.Time, span trace.Span, tryBackend func(ctx context.Context, backendName string, backend ObjectBackend) (int64, error)) (string, error) {
	type broadcastResult struct {
		name string
		size int64
		err  error
	}

	launched := 0
	ch := make(chan broadcastResult, len(m.order))

	for _, name := range m.order {
		backend, ok := m.backends[name]
		if !ok {
			continue
		}
		launched++
		go func(beName string, be ObjectBackend) {
			tctx, tcancel := m.withTimeout(ctx)
			size, err := tryBackend(tctx, beName, be)
			if err != nil {
				tcancel()
			}
			// On success, don't cancel — the body may still be streaming.
			// The timeout context expires naturally; losing goroutines'
			// bodies are closed by the once.Do guard in the callback.
			ch <- broadcastResult{name: beName, size: size, err: err}
		}(name, backend)
	}

	var lastErr error
	for range launched {
		r := <-ch
		if r.err != nil {
			lastErr = r.err
			continue
		}

		// First success — don't cancel the winning context
		m.cache.Set(key, r.name)
		m.recordOperation(operation, r.name, start, nil)
		span.SetAttributes(telemetry.AttrBackendName.String(r.name))
		span.SetAttributes(telemetry.AttrObjectSize.Int64(r.size))
		span.SetAttributes(attribute.Bool("s3proxy.parallel_broadcast", true))
		span.SetStatus(codes.Ok, "")
		return r.name, nil
	}

	if lastErr != nil {
		span.SetStatus(codes.Error, lastErr.Error())
		span.RecordError(lastErr)
		return "", fmt.Errorf("all backends failed during degraded read: %w", lastErr)
	}

	span.SetStatus(codes.Error, "no backends available")
	return "", ErrObjectNotFound
}

// -------------------------------------------------------------------------
// READ OPERATIONS
// -------------------------------------------------------------------------

// GetObject retrieves an object from the backend where it's stored. Tries the
// primary copy first, then falls back to replicas if the primary fails. When
// the object is encrypted, the response body is transparently decrypted and
// the reported size reflects the original plaintext size.
func (m *BackendManager) GetObject(ctx context.Context, key string, rangeHeader string) (*GetObjectResult, error) {
	var result *GetObjectResult
	var once sync.Once // protects result write when parallel broadcast is enabled

	// Resolve locations upfront so encryption metadata is available after read.
	locations, locErr := m.store.GetAllObjectLocations(ctx, key)

	backendName, err := m.withReadFailover(ctx, "GetObject", key, func(ctx context.Context, beName string, backend ObjectBackend) (int64, error) {
		if !m.usage.WithinLimits(beName, 1, 0, 0) {
			return 0, fmt.Errorf("backend %s: %w", beName, errUsageLimitSkip)
		}

		// Find encryption metadata for this backend's copy
		var loc *ObjectLocation
		if m.encryptor != nil && locErr == nil {
			for i := range locations {
				if locations[i].BackendName == beName {
					loc = &locations[i]
					break
				}
			}
		}

		// Translate plaintext range to ciphertext range for encrypted objects
		actualRange := rangeHeader
		var rng *encryption.RangeResult
		var ptStart, ptEnd int64
		if loc != nil && loc.Encrypted && rangeHeader != "" {
			var ok bool
			ptStart, ptEnd, ok = parsePlaintextRange(rangeHeader, loc.PlaintextSize)
			if ok {
				rng, _ = encryption.CiphertextRange(ptStart, ptEnd, m.encryptor.ChunkSize())
				if rng != nil {
					actualRange = rng.BackendRange
				}
			}
		}

		r, err := backend.GetObject(ctx, key, actualRange)
		if err != nil {
			m.usage.Record(beName, 1, 0, 0)
			return 0, err
		}
		if !m.usage.WithinLimits(beName, 1, r.Size, 0) {
			_ = r.Body.Close()
			m.usage.Record(beName, 1, 0, 0)
			return 0, fmt.Errorf("backend %s egress: %w", beName, errUsageLimitSkip)
		}

		// Decrypt the response body if the object is encrypted
		if loc != nil && loc.Encrypted && m.encryptor != nil {
			baseNonce, wrappedDEK, unpackErr := encryption.UnpackKeyData(loc.EncryptionKey)
			if unpackErr != nil {
				telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt", "unpack_failed").Inc()
				_ = r.Body.Close()
				return 0, fmt.Errorf("unpack key data: %w", unpackErr)
			}

			if rng != nil {
				// Range request: decrypt only the fetched chunks
				plainReader, plainLen, decErr := m.encryptor.DecryptRange(ctx, r.Body, wrappedDEK, loc.KeyID, rng, baseNonce)
				if decErr != nil {
					telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt_range", "decrypt_failed").Inc()
					_ = r.Body.Close()
					return 0, fmt.Errorf("decrypt range: %w", decErr)
				}
				telemetry.EncryptionOpsTotal.WithLabelValues("decrypt_range").Inc()
				r.Body = wrapReader(plainReader, r.Body)
				r.Size = plainLen
				r.ContentRange = fmt.Sprintf("bytes %d-%d/%d", ptStart, ptEnd, loc.PlaintextSize)
			} else {
				// Full read: decrypt the entire ciphertext stream
				decrypted, decErr := m.encryptor.Decrypt(ctx, r.Body, wrappedDEK, loc.KeyID)
				if decErr != nil {
					telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt", "decrypt_failed").Inc()
					_ = r.Body.Close()
					return 0, fmt.Errorf("decrypt: %w", decErr)
				}
				telemetry.EncryptionOpsTotal.WithLabelValues("decrypt").Inc()
				r.Body = wrapReader(decrypted, r.Body)
				r.Size = loc.PlaintextSize
			}
		}

		once.Do(func() { result = r })
		if result != r {
			_ = r.Body.Close()
		}
		return r.Size, nil
	})
	if err != nil {
		return nil, err
	}
	m.usage.Record(backendName, 1, result.Size, 0)

	audit.Log(ctx, "storage.GetObject",
		slog.String("key", key),
		slog.String("backend", backendName),
		slog.Int64("size", result.Size),
	)

	return result, nil
}

// HeadObject retrieves object metadata. Tries the primary copy first, then
// falls back to replicas if the primary fails. When the object is encrypted,
// the reported size reflects the original plaintext size.
func (m *BackendManager) HeadObject(ctx context.Context, key string) (*HeadObjectResult, error) {
	var result *HeadObjectResult
	var once sync.Once // protects result write when parallel broadcast is enabled

	// Resolve locations upfront so encryption metadata is available.
	locations, locErr := m.store.GetAllObjectLocations(ctx, key)

	backendName, err := m.withReadFailover(ctx, "HeadObject", key, func(ctx context.Context, beName string, backend ObjectBackend) (int64, error) {
		if !m.usage.WithinLimits(beName, 1, 0, 0) {
			return 0, fmt.Errorf("backend %s: %w", beName, errUsageLimitSkip)
		}
		r, err := backend.HeadObject(ctx, key)
		if err != nil {
			m.usage.Record(beName, 1, 0, 0) // API call was made even on failure
			return 0, err
		}

		// Return plaintext size for encrypted objects
		if m.encryptor != nil && locErr == nil {
			for i := range locations {
				if locations[i].BackendName == beName && locations[i].Encrypted {
					r.Size = locations[i].PlaintextSize
					break
				}
			}
		}

		once.Do(func() { result = r })
		return r.Size, nil
	})
	if err != nil {
		return nil, err
	}
	m.usage.Record(backendName, 1, 0, 0)

	audit.Log(ctx, "storage.HeadObject",
		slog.String("key", key),
		slog.String("backend", backendName),
		slog.Int64("size", result.Size),
	)

	return result, nil
}

// -------------------------------------------------------------------------
// COPY
// -------------------------------------------------------------------------

// CopyObject copies an object from sourceKey to destKey. Streams the source
// through a pipe to avoid buffering the entire object. Supports cross-backend
// copies and read failover from replicas.
func (m *BackendManager) CopyObject(ctx context.Context, sourceKey, destKey string) (string, error) {
	const operation = "CopyObject"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		attribute.String("s3.source_key", sourceKey),
		attribute.String("s3.dest_key", destKey),
	)
	defer span.End()

	// --- Find all source locations (for failover) ---
	locations, err := m.store.GetAllObjectLocations(ctx, sourceKey)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			span.SetStatus(codes.Error, "source object not found")
			return "", err
		}
		return "", m.classifyWriteError(span, operation, err)
	}

	// --- Get source metadata (try each copy, skip over-limit backends) ---
	var size int64
	var contentType string
	var metadata map[string]string
	var srcFound bool
	var srcEnc *EncryptionMeta
	for _, loc := range locations {
		if !m.usage.WithinLimits(loc.BackendName, 1, 0, 0) {
			continue
		}
		backend, ok := m.backends[loc.BackendName]
		if !ok {
			continue
		}
		bctx, bcancel := m.withTimeout(ctx)
		headResult, err := backend.HeadObject(bctx, sourceKey)
		bcancel()
		if err != nil {
			continue
		}
		size = headResult.Size
		contentType = headResult.ContentType
		metadata = headResult.Metadata
		srcFound = true
		if loc.Encrypted {
			srcEnc = &EncryptionMeta{
				Encrypted:     true,
				EncryptionKey: loc.EncryptionKey,
				KeyID:         loc.KeyID,
				PlaintextSize: loc.PlaintextSize,
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

	// --- Find destination backend with available quota and usage limits ---
	destEligible := m.excludeDraining(m.usage.BackendsWithinLimits(m.order, 1, 0, size))
	if len(destEligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "write").Inc()
		span.SetStatus(codes.Error, "usage limits exceeded on all backends")
		return "", ErrInsufficientStorage
	}
	destBackendName, err := m.selectBackendForWrite(ctx, size, destEligible)
	if err != nil {
		return "", m.classifyWriteError(span, operation, err)
	}

	destBackend, err := m.getBackend(destBackendName)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	// --- Stream source to destination via pipe (with failover) ---
	pr, pw := io.Pipe()
	srcBackendCh := make(chan string, 1)

	go func() {
		defer func() { _ = pw.Close() }()
		for _, loc := range locations {
			if !m.usage.WithinLimits(loc.BackendName, 1, size, 0) {
				continue
			}
			srcBackend, ok := m.backends[loc.BackendName]
			if !ok {
				continue
			}
			bctx, bcancel := m.withTimeout(ctx)
			result, err := srcBackend.GetObject(bctx, sourceKey, "")
			if err != nil {
				bcancel()
				continue
			}
			srcBackendCh <- loc.BackendName
			_, copyErr := io.Copy(pw, result.Body)
			_ = result.Body.Close()
			bcancel()
			if copyErr != nil {
				pw.CloseWithError(fmt.Errorf("failed to stream source: %w", copyErr))
			}
			return
		}
		close(srcBackendCh)
		pw.CloseWithError(fmt.Errorf("failed to read source from any copy"))
	}()

	// Streamed from pipe; deadline governed by the caller's context.
	etag, err := destBackend.PutObject(ctx, destKey, pr, size, contentType, metadata)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return "", fmt.Errorf("failed to write destination: %w", err)
	}

	// --- Record destination location and update quota ---
	// Preserve encryption metadata: ciphertext is copied as-is so the
	// destination keeps the same wrapped DEK and key ID.
	if err := m.recordObjectOrCleanup(ctx, span, destBackend, destKey, destBackendName, size, srcEnc); err != nil {
		return "", err
	}

	// --- Invalidate location cache ---
	m.cache.Delete(destKey)

	m.recordOperation(operation, destBackendName, start, nil)
	var srcName string
	if sn, ok := <-srcBackendCh; ok {
		srcName = sn
		m.usage.Record(srcName, 1, size, 0) // source: Get + egress
	}
	m.usage.Record(destBackendName, 1, 0, size) // dest: Put + ingress

	audit.Log(ctx, "storage.CopyObject",
		slog.String("source_key", sourceKey),
		slog.String("dest_key", destKey),
		slog.String("source_backend", srcName),
		slog.String("dest_backend", destBackendName),
		slog.Int64("size", size),
	)

	span.SetStatus(codes.Ok, "")
	return etag, nil
}

// -------------------------------------------------------------------------
// DELETE
// -------------------------------------------------------------------------

// DeleteObject removes an object from the backend where it's stored.
func (m *BackendManager) DeleteObject(ctx context.Context, key string) error {
	const operation = "DeleteObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()

	// --- Delete all copies from store ---
	copies, err := m.store.DeleteObject(ctx, key)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			// Object not in our tracking - treat as success (idempotent delete)
			span.SetStatus(codes.Ok, "object not found - treating as success")
			return nil
		}
		return m.classifyWriteError(span, operation, err)
	}

	span.SetAttributes(attribute.Int("copies.deleted", len(copies)))

	// --- Invalidate location cache ---
	m.cache.Delete(key)

	// --- Delete from each backend that held a copy ---
	for _, copy := range copies {
		backend, ok := m.backends[copy.BackendName]
		if !ok {
			slog.Warn("Backend not found for delete",
				"backend", copy.BackendName, "key", key)
			continue
		}
		m.deleteOrEnqueue(ctx, backend, copy.BackendName, key, "delete_failed")
	}

	// --- Record metrics (use first copy's backend for primary) ---
	if len(copies) > 0 {
		m.recordOperation(operation, copies[0].BackendName, start, nil)
	}
	for _, c := range copies {
		m.usage.Record(c.BackendName, 1, 0, 0)
	}

	audit.Log(ctx, "storage.DeleteObject",
		slog.String("key", key),
		slog.Int("copies_deleted", len(copies)),
	)

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
func (m *BackendManager) DeleteObjects(ctx context.Context, keys []string) []DeleteObjectResult {
	const operation = "DeleteObjects"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		attribute.Int("s3.batch_size", len(keys)),
	)
	defer span.End()

	results := make([]DeleteObjectResult, len(keys))

	// Remove each key from the DB and collect backend copies that need
	// physical deletion afterwards.
	type pendingBackendDelete struct {
		key    string
		copies []DeletedCopy
	}
	var pending []pendingBackendDelete

	for i, key := range keys {
		results[i].Key = key

		copies, err := m.store.DeleteObject(ctx, key)
		if err != nil {
			if errors.Is(err, ErrObjectNotFound) {
				continue // not-found treated as success
			}
			results[i].Err = m.classifyWriteError(span, operation, err)
			continue
		}

		m.cache.Delete(key)

		if len(copies) > 0 {
			pending = append(pending, pendingBackendDelete{key: key, copies: copies})
		}
	}

	// Delete from backends concurrently, capped at 10 in-flight calls.
	const maxConcurrency = 10
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var backendErrors []error

	for _, pd := range pending {
		for _, cp := range pd.copies {
			backend, ok := m.backends[cp.BackendName]
			if !ok {
				slog.Warn("Backend not found for batch delete",
					"backend", cp.BackendName, "key", pd.key)
				continue
			}

			wg.Add(1)
			sem <- struct{}{}

			go func(beName, key string, be ObjectBackend) {
				defer wg.Done()
				defer func() { <-sem }()

				err := m.deleteWithTimeout(ctx, be, key)
				if err != nil {
					slog.Warn("Failed to delete object from backend (batch)",
						"backend", beName, "key", key, "error", err)
					m.enqueueCleanup(ctx, beName, key, "batch_delete_failed")
					mu.Lock()
					backendErrors = append(backendErrors, err)
					mu.Unlock()
				}
			}(cp.BackendName, pd.key, backend)
		}

		for _, cp := range pd.copies {
			m.usage.Record(cp.BackendName, 1, 0, 0)
		}
	}

	wg.Wait()

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

	m.recordOperation(operation, "", start, nil)

	audit.Log(ctx, "storage.DeleteObjects",
		slog.Int("total_keys", len(keys)),
		slog.Int("deleted", successCount),
		slog.Int("errors", errorCount),
	)

	span.SetAttributes(
		attribute.Int("s3.deleted_count", successCount),
		attribute.Int("s3.error_count", errorCount),
	)
	span.SetStatus(codes.Ok, "")

	return results
}

// -------------------------------------------------------------------------
// LIST
// -------------------------------------------------------------------------

// ListObjectsV2Result holds the processed result for the S3 ListObjectsV2 response.
type ListObjectsV2Result struct {
	Objects               []ObjectLocation
	CommonPrefixes        []string
	IsTruncated           bool
	NextContinuationToken string
	KeyCount              int
}

// ListObjects returns objects matching the given prefix with optional delimiter
// support for virtual directory grouping. When a delimiter is set, many raw
// objects may collapse into a single CommonPrefix, so we loop-fetch from the
// store until maxKeys post-grouping items are collected or the store is
// exhausted.
func (m *BackendManager) ListObjects(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*ListObjectsV2Result, error) {
	const operation = "ListObjects"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		attribute.String("s3.prefix", prefix),
		attribute.String("s3.delimiter", delimiter),
		attribute.Int("s3.max_keys", maxKeys),
	)
	defer span.End()

	result := &ListObjectsV2Result{}
	cursor := startAfter
	seen := make(map[string]bool) // tracks emitted common prefixes across fetches
	lastStoreTruncated := false   // whether the most recent store page had more data

	const maxPages = 100 // cap DB round trips per request
	for page := 0; page < maxPages && result.KeyCount < maxKeys; page++ {
		storeResult, err := m.store.ListObjects(ctx, prefix, cursor, maxKeys)
		if err != nil {
			if errors.Is(err, ErrDBUnavailable) {
				span.SetStatus(codes.Error, "database unavailable")
				return nil, &S3Error{StatusCode: 503, Code: "ServiceUnavailable", Message: "listing unavailable during database outage"}
			}
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}

		if len(storeResult.Objects) == 0 {
			break
		}
		lastStoreTruncated = storeResult.IsTruncated

		// lastKey tracks the most recently consumed object key so the
		// continuation token lands after the entire prefix group, not
		// in the middle of one.
		var lastKey string
		for _, obj := range storeResult.Objects {
			// Check CommonPrefix membership before the limit check so
			// objects that collapse into an already-counted prefix are
			// skipped without triggering premature truncation.
			if delimiter != "" {
				rest := obj.ObjectKey[len(prefix):]
				idx := strings.Index(rest, delimiter)
				if idx >= 0 {
					cp := obj.ObjectKey[:len(prefix)+idx+len(delimiter)]
					if seen[cp] {
						// Same prefix already counted — skip silently
						lastKey = obj.ObjectKey
						continue
					}
					// New prefix would add an entry — enforce limit
					if result.KeyCount >= maxKeys {
						result.IsTruncated = true
						result.NextContinuationToken = lastKey
						break
					}
					seen[cp] = true
					result.CommonPrefixes = append(result.CommonPrefixes, cp)
					result.KeyCount++
					lastKey = obj.ObjectKey
					continue
				}
			}

			// Regular object — enforce limit before adding
			if result.KeyCount >= maxKeys {
				result.IsTruncated = true
				result.NextContinuationToken = lastKey
				break
			}

			result.Objects = append(result.Objects, obj)
			result.KeyCount++
			lastKey = obj.ObjectKey
		}

		if result.IsTruncated || !storeResult.IsTruncated {
			break
		}
		cursor = storeResult.Objects[len(storeResult.Objects)-1].ObjectKey

		// If this is the last allowed page and the store has more data,
		// mark as truncated so the client paginates back.
		if page == maxPages-1 && storeResult.IsTruncated && !result.IsTruncated {
			result.IsTruncated = true
			result.NextContinuationToken = cursor
		}
	}

	// When we consumed exactly maxKeys entries from a single store page
	// and the store reported more data, the inner loop never tried to add
	// a (maxKeys+1)th entry so IsTruncated was never set. Detect this and
	// mark the result as truncated so the client paginates correctly.
	if !result.IsTruncated && lastStoreTruncated && result.KeyCount >= maxKeys {
		result.IsTruncated = true
		result.NextContinuationToken = cursor
	}

	m.recordOperation(operation, "", start, nil)

	audit.Log(ctx, "storage.ListObjects",
		slog.String("prefix", prefix),
		slog.Int("key_count", result.KeyCount),
		slog.Bool("truncated", result.IsTruncated),
	)

	span.SetStatus(codes.Ok, "")
	span.SetAttributes(attribute.Int("s3.key_count", result.KeyCount))
	return result, nil
}

// -------------------------------------------------------------------------
// RANGE PARSING
// -------------------------------------------------------------------------

// parsePlaintextRange extracts the start and end byte offsets from an HTTP
// Range header value (e.g., "bytes=0-99"). Suffix ranges and open-ended
// ranges are resolved against plaintextSize.
func parsePlaintextRange(rangeHeader string, plaintextSize int64) (start, end int64, ok bool) {
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, false
	}
	spec := rangeHeader[len("bytes="):]
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}

	if parts[0] == "" {
		// Suffix range: bytes=-N
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		start = plaintextSize - n
		if start < 0 {
			start = 0
		}
		return start, plaintextSize - 1, true
	}

	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}

	if parts[1] == "" {
		// Open-ended: bytes=N-
		return start, plaintextSize - 1, true
	}

	end, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}

	return start, end, true
}

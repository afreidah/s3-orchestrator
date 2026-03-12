// -------------------------------------------------------------------------------
// Object Manager - PUT, GET, HEAD, DELETE, COPY, DeleteObjects, ListObjects
//
// Author: Alex Freidah
//
// Object-level CRUD operations. Handles backend selection via routing strategy,
// read failover across replicas, broadcast reads during degraded mode, and
// usage limit enforcement on reads and writes. DeleteObjects provides batch
// deletion with concurrent backend I/O.
// -------------------------------------------------------------------------------

package storage

import (
	"bytes"
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
	"github.com/afreidah/s3-orchestrator/internal/bufpool"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/workerpool"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ObjectManager handles object-level CRUD operations with read failover,
// broadcast reads during degraded mode, and location caching.
type ObjectManager struct {
	*backendCore
	encryptor         *encryption.Encryptor
	cache             *LocationCache
	parallelBroadcast bool
}

// NewObjectManager creates an ObjectManager sharing the given core infrastructure.
func NewObjectManager(core *backendCore, encryptor *encryption.Encryptor, cache *LocationCache, parallelBroadcast bool) *ObjectManager {
	return &ObjectManager{
		backendCore:       core,
		encryptor:         encryptor,
		cache:             cache,
		parallelBroadcast: parallelBroadcast,
	}
}

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
// PRE-FLIGHT CHECKS
// -------------------------------------------------------------------------

// CanAcceptWrite reports whether any backend can accept a write of the given
// size. Used by the HTTP handler to reject uploads before the request body
// is transmitted (Expect: 100-Continue support).
func (o *ObjectManager) CanAcceptWrite(size int64) bool {
	eligible := o.excludeUnhealthy(o.excludeDraining(
		o.usage.BackendsWithinLimits(o.order, 1, 0, size)))
	return len(eligible) > 0
}

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
	eligible := o.excludeUnhealthy(o.excludeDraining(o.usage.BackendsWithinLimits(o.order, 1, 0, size)))
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "write").Inc()
		span.SetStatus(codes.Error, "usage limits exceeded on all backends")
		return "", ErrInsufficientStorage
	}

	// --- Buffer body for retry ---
	// io.Reader is single-use; buffer the plaintext so we can replay on failover.
	var buf bytes.Buffer
	if _, err := bufpool.Copy(&buf, body); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("buffer request body: %w", err)
	}
	bodyBytes := buf.Bytes()

	// --- Try eligible backends with failover ---
	var failedBackends []string
	var lastErr error
	for len(eligible) > 0 {
		backendName, err := o.selectBackendForWrite(ctx, size, eligible)
		if err != nil {
			return "", o.classifyWriteError(span, operation, err)
		}

		span.SetAttributes(telemetry.AttrBackendName.String(backendName))

		backend, err := o.getBackend(backendName)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return "", err
		}

		// --- Encrypt if enabled (re-encrypt per attempt — unique nonce each time) ---
		var enc *EncryptionMeta
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
			enc = &EncryptionMeta{
				Encrypted:     true,
				EncryptionKey: encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK),
				KeyID:         encResult.KeyID,
				PlaintextSize: size,
			}
		}

		// --- Upload to backend ---
		bctx, bcancel := o.withTimeout(ctx)
		etag, err := backend.PutObject(bctx, key, uploadBody, uploadSize, contentType, metadata)
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

			slog.Warn("PutObject: backend write failed, trying next",
				"key", key, "failed_backend", backendName, "error", err,
				"remaining_backends", len(eligible))
			continue
		}

		// --- Record object location and update quota ---
		if err := o.recordObjectOrCleanup(ctx, span, backend, key, backendName, uploadSize, enc); err != nil {
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
			span.SetAttributes(attribute.Bool("s3proxy.write_failover", true))
			span.SetAttributes(attribute.Int("s3proxy.write_failover_attempts", len(failedBackends)))
		}

		audit.Log(ctx, "storage.PutObject",
			slog.String("key", key),
			slog.String("backend", backendName),
			slog.Int64("size", size),
		)

		span.SetStatus(codes.Ok, "")
		return etag, nil
	}

	// All eligible backends exhausted
	span.SetStatus(codes.Error, lastErr.Error())
	span.RecordError(lastErr)
	return "", lastErr
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
func (o *ObjectManager) withReadFailover(ctx context.Context, operation, key string, tryBackend func(ctx context.Context, backendName string, backend ObjectBackend) (int64, error)) (string, error) {
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()

	locations, err := o.store.GetAllObjectLocations(ctx, key)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			span.SetStatus(codes.Error, "object not found")
			return "", err
		}
		if errors.Is(err, ErrDBUnavailable) {
			return o.broadcastRead(ctx, operation, key, start, span, tryBackend)
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return "", fmt.Errorf("failed to find object location: %w", err)
	}

	var lastErr error
	var limitSkips int
	for i, loc := range locations {
		span.SetAttributes(telemetry.AttrBackendName.String(loc.BackendName))

		backend, ok := o.backends[loc.BackendName]
		if !ok {
			lastErr = fmt.Errorf("backend %s not found", loc.BackendName)
			continue
		}

		bctx, bcancel := o.withTimeout(ctx)
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

		o.recordOperation(operation, loc.BackendName, start, nil)
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
func (o *ObjectManager) broadcastRead(ctx context.Context, operation, key string, start time.Time, span trace.Span, tryBackend func(ctx context.Context, backendName string, backend ObjectBackend) (int64, error)) (string, error) {
	span.SetAttributes(attribute.Bool("s3proxy.degraded_mode", true))
	telemetry.DegradedReadsTotal.WithLabelValues(operation).Inc()

	// --- Check location cache first ---
	if cachedBackend, ok := o.cache.Get(key); ok {
		if backend, exists := o.backends[cachedBackend]; exists {
			bctx, bcancel := o.withTimeout(ctx)
			size, err := tryBackend(bctx, cachedBackend, backend)
			if err == nil {
				o.recordOperation(operation, cachedBackend, start, nil)
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

	if o.parallelBroadcast {
		return o.parallelBroadcastRead(ctx, operation, key, start, span, tryBackend)
	}
	return o.sequentialBroadcastRead(ctx, operation, key, start, span, tryBackend)
}

// sequentialBroadcastRead tries each backend in order until one succeeds.
func (o *ObjectManager) sequentialBroadcastRead(ctx context.Context, operation, key string, start time.Time, span trace.Span, tryBackend func(ctx context.Context, backendName string, backend ObjectBackend) (int64, error)) (string, error) {
	var lastErr error
	for _, name := range o.order {
		backend, ok := o.backends[name]
		if !ok {
			continue
		}
		bctx, bcancel := o.withTimeout(ctx)
		size, err := tryBackend(bctx, name, backend)
		if err != nil {
			bcancel()
			lastErr = err
			continue
		}

		// Success — cache the result for future degraded reads
		o.cache.Set(key, name)
		o.recordOperation(operation, name, start, nil)
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
func (o *ObjectManager) parallelBroadcastRead(ctx context.Context, operation, key string, start time.Time, span trace.Span, tryBackend func(ctx context.Context, backendName string, backend ObjectBackend) (int64, error)) (string, error) {
	type broadcastResult struct {
		name string
		size int64
		err  error
	}

	launched := 0
	ch := make(chan broadcastResult, len(o.order))

	for _, name := range o.order {
		backend, ok := o.backends[name]
		if !ok {
			continue
		}
		launched++
		go func(beName string, be ObjectBackend) {
			tctx, tcancel := o.withTimeout(ctx)
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
		o.cache.Set(key, r.name)
		o.recordOperation(operation, r.name, start, nil)
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
func (o *ObjectManager) GetObject(ctx context.Context, key string, rangeHeader string) (*GetObjectResult, error) {
	var result *GetObjectResult
	var once sync.Once // protects result write when parallel broadcast is enabled

	// Resolve locations upfront so encryption metadata is available after read.
	locations, locErr := o.store.GetAllObjectLocations(ctx, key)

	backendName, err := o.withReadFailover(ctx, "GetObject", key, func(ctx context.Context, beName string, backend ObjectBackend) (int64, error) {
		if !o.usage.WithinLimits(beName, 1, 0, 0) {
			return 0, fmt.Errorf("backend %s: %w", beName, errUsageLimitSkip)
		}

		// Find encryption metadata for this backend's copy
		var loc *ObjectLocation
		if o.encryptor != nil && locErr == nil {
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
				rng, _ = encryption.CiphertextRange(ptStart, ptEnd, o.encryptor.ChunkSize())
				if rng != nil {
					actualRange = rng.BackendRange
				}
			}
		}

		r, err := backend.GetObject(ctx, key, actualRange)
		if err != nil {
			o.usage.Record(beName, 1, 0, 0)
			return 0, err
		}
		if !o.usage.WithinLimits(beName, 1, r.Size, 0) {
			_ = r.Body.Close()
			o.usage.Record(beName, 1, 0, 0)
			return 0, fmt.Errorf("backend %s egress: %w", beName, errUsageLimitSkip)
		}

		// Decrypt the response body if the object is encrypted
		if loc != nil && loc.Encrypted && o.encryptor != nil {
			baseNonce, wrappedDEK, unpackErr := encryption.UnpackKeyData(loc.EncryptionKey)
			if unpackErr != nil {
				telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt", "unpack_failed").Inc()
				_ = r.Body.Close()
				return 0, fmt.Errorf("unpack key data: %w", unpackErr)
			}

			if rng != nil {
				// Range request: decrypt only the fetched chunks
				plainReader, plainLen, decErr := o.encryptor.DecryptRange(ctx, r.Body, wrappedDEK, loc.KeyID, rng, baseNonce)
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
				decrypted, decErr := o.encryptor.Decrypt(ctx, r.Body, wrappedDEK, loc.KeyID)
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
	o.usage.Record(backendName, 1, result.Size, 0)

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
func (o *ObjectManager) HeadObject(ctx context.Context, key string) (*HeadObjectResult, error) {
	var result *HeadObjectResult
	var once sync.Once // protects result write when parallel broadcast is enabled

	// Resolve locations upfront so encryption metadata is available.
	locations, locErr := o.store.GetAllObjectLocations(ctx, key)

	backendName, err := o.withReadFailover(ctx, "HeadObject", key, func(ctx context.Context, beName string, backend ObjectBackend) (int64, error) {
		if !o.usage.WithinLimits(beName, 1, 0, 0) {
			return 0, fmt.Errorf("backend %s: %w", beName, errUsageLimitSkip)
		}
		r, err := backend.HeadObject(ctx, key)
		if err != nil {
			o.usage.Record(beName, 1, 0, 0) // API call was made even on failure
			return 0, err
		}

		// Return plaintext size for encrypted objects
		if o.encryptor != nil && locErr == nil {
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
	o.usage.Record(backendName, 1, 0, 0)

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
func (o *ObjectManager) CopyObject(ctx context.Context, sourceKey, destKey string) (string, error) {
	const operation = "CopyObject"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		attribute.String("s3.source_key", sourceKey),
		attribute.String("s3.dest_key", destKey),
	)
	defer span.End()

	// --- Find all source locations (for failover) ---
	locations, err := o.store.GetAllObjectLocations(ctx, sourceKey)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
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
	var srcEnc *EncryptionMeta
	for _, loc := range locations {
		if !o.usage.WithinLimits(loc.BackendName, 1, 0, 0) {
			continue
		}
		backend, ok := o.backends[loc.BackendName]
		if !ok {
			continue
		}
		bctx, bcancel := o.withTimeout(ctx)
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

	// --- Find destination backend with available quota, usage limits, exclude unhealthy ---
	destEligible := o.excludeUnhealthy(o.excludeDraining(o.usage.BackendsWithinLimits(o.order, 1, 0, size)))
	if len(destEligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "write").Inc()
		span.SetStatus(codes.Error, "usage limits exceeded on all backends")
		return "", ErrInsufficientStorage
	}
	destBackendName, err := o.selectBackendForWrite(ctx, size, destEligible)
	if err != nil {
		return "", o.classifyWriteError(span, operation, err)
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
		for _, loc := range locations {
			if !o.usage.WithinLimits(loc.BackendName, 1, size, 0) {
				continue
			}
			srcBackend, ok := o.backends[loc.BackendName]
			if !ok {
				continue
			}
			bctx, bcancel := o.withTimeout(ctx)
			result, err := srcBackend.GetObject(bctx, sourceKey, "")
			if err != nil {
				bcancel()
				continue
			}
			srcBackendCh <- loc.BackendName
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
		if errors.Is(err, ErrObjectNotFound) {
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
	workerpool.Run(ctx, len(copies), copies, func(ctx context.Context, cp DeletedCopy) {
		backend, ok := o.backends[cp.BackendName]
		if !ok {
			slog.Warn("Backend not found for delete",
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

		copies, err := o.store.DeleteObject(ctx, key)
		if err != nil {
			if errors.Is(err, ErrObjectNotFound) {
				continue // not-found treated as success
			}
			results[i].Err = o.classifyWriteError(span, operation, err)
			continue
		}

		o.cache.Delete(key)

		if len(copies) > 0 {
			pending = append(pending, pendingBackendDelete{key: key, copies: copies})
		}
	}

	// Flatten pending deletes into a single work slice for the pool
	type batchDeleteItem struct {
		key       string
		backend   ObjectBackend
		beName    string
		sizeBytes int64
	}
	var deleteItems []batchDeleteItem
	for _, pd := range pending {
		for _, cp := range pd.copies {
			backend, ok := o.backends[cp.BackendName]
			if !ok {
				slog.Warn("Backend not found for batch delete",
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
			slog.Warn("Failed to delete object from backend (batch)",
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
func (o *ObjectManager) ListObjects(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*ListObjectsV2Result, error) {
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
		storeResult, err := o.store.ListObjects(ctx, prefix, cursor, maxKeys)
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

	o.recordOperation(operation, "", start, nil)

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

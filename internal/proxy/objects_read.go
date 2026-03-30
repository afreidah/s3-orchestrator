// -------------------------------------------------------------------------------
// Object Manager - Read Operations (GET, HEAD, LIST)
//
// Author: Alex Freidah
//
// Read failover, broadcast reads, GetObject, HeadObject, ListObjects.
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
	"strconv"
	"strings"
	"sync"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/store"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

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
func (o *ObjectManager) withReadFailover(ctx context.Context, operation, key string, tryBackend func(ctx context.Context, backendName string, backend s3be.ObjectBackend) (int64, error)) (string, error) {
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()

	locations, err := o.store.GetAllObjectLocations(ctx, key)
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			span.SetStatus(codes.Error, "object not found")
			return "", err
		}
		if errors.Is(err, store.ErrDBUnavailable) {
			return o.broadcastRead(ctx, operation, key, start, span, tryBackend)
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return "", fmt.Errorf("failed to find object location: %w", err)
	}

	var lastErr error
	var limitSkips int
	for i := range locations {
		span.SetAttributes(telemetry.AttrBackendName.String(locations[i].BackendName))

		backend, ok := o.backends[locations[i].BackendName]
		if !ok {
			lastErr = fmt.Errorf("backend %s not found", locations[i].BackendName)
			continue
		}

		bctx, bcancel := o.withTimeout(ctx)
		size, err := tryBackend(bctx, locations[i].BackendName, backend)
		if err != nil {
			bcancel()
			lastErr = err
			if errors.Is(err, errUsageLimitSkip) {
				limitSkips++
			}
			if i < len(locations)-1 {
				slog.WarnContext(ctx, operation+": copy failed, trying next",
					"key", key, "failed_backend", locations[i].BackendName, "error", err)
			}
			continue
		}

		o.recordOperation(operation, locations[i].BackendName, start, nil)
		if i > 0 {
			span.SetAttributes(telemetry.AttrFailover.Bool(true))
		}
		span.SetAttributes(telemetry.AttrObjectSize.Int64(size))
		span.SetStatus(codes.Ok, "")
		return locations[i].BackendName, nil
	}

	// All copies were on over-limit backends — return the usage limit error
	if limitSkips > 0 && limitSkips == len(locations) {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "read").Inc()
		span.SetStatus(codes.Error, "all copies over usage limit")
		return "", store.ErrUsageLimitExceeded
	}

	span.SetStatus(codes.Error, lastErr.Error())
	span.RecordError(lastErr)
	return "", lastErr
}

// broadcastRead tries all backends when the DB is unavailable. Checks the
// location cache first for a known-good backend, then dispatches to either
// parallel or sequential broadcast based on configuration.
func (o *ObjectManager) broadcastRead(ctx context.Context, operation, key string, start time.Time, span trace.Span, tryBackend func(ctx context.Context, backendName string, backend s3be.ObjectBackend) (int64, error)) (string, error) {
	span.SetAttributes(telemetry.AttrDegradedMode.Bool(true))
	telemetry.DegradedReadsTotal.WithLabelValues(operation).Inc()

	// --- Check location cache first ---
	if cachedBackend, ok := o.cache.Get(key); ok {
		if backend, exists := o.backends[cachedBackend]; exists {
			bctx, bcancel := o.withTimeout(ctx)
			size, err := tryBackend(bctx, cachedBackend, backend)
			if err == nil {
				o.recordOperation(operation, cachedBackend, start, nil)
				span.SetAttributes(telemetry.AttrCacheHit.Bool(true))
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
func (o *ObjectManager) sequentialBroadcastRead(ctx context.Context, operation, key string, start time.Time, span trace.Span, tryBackend func(ctx context.Context, backendName string, backend s3be.ObjectBackend) (int64, error)) (string, error) {
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
	return "", store.ErrObjectNotFound
}

// parallelBroadcastRead fans out to all backends concurrently and returns
// the first successful result. A background goroutine drains remaining
// results and cancels losing contexts so backend connections are released
// promptly rather than lingering until their timeout expires.
func (o *ObjectManager) parallelBroadcastRead(ctx context.Context, operation, key string, start time.Time, span trace.Span, tryBackend func(ctx context.Context, backendName string, backend s3be.ObjectBackend) (int64, error)) (string, error) {
	type broadcastResult struct {
		name   string
		size   int64
		err    error
		cancel context.CancelFunc
	}

	launched := 0
	ch := make(chan broadcastResult, len(o.order))

	for _, name := range o.order {
		backend, ok := o.backends[name]
		if !ok {
			continue
		}
		launched++
		go func(beName string, be s3be.ObjectBackend) {
			tctx, tcancel := o.withTimeout(ctx)
			size, err := tryBackend(tctx, beName, be)
			if err != nil {
				tcancel()
				ch <- broadcastResult{name: beName, err: err}
				return
			}
			// On success, send the cancel func so the caller can decide
			// whether to keep the context alive (winner) or cancel it (loser).
			ch <- broadcastResult{name: beName, size: size, cancel: tcancel}
		}(name, backend)
	}

	var lastErr error
	for received := 0; received < launched; received++ { //nolint:intrange // received used in arithmetic below
		r := <-ch
		if r.err != nil {
			lastErr = r.err
			continue
		}

		// First success — drain remaining results in the background
		// to cancel loser contexts promptly.
		remaining := launched - received - 1
		if remaining > 0 {
			go func() {
				defer func() { recover() }() //nolint:errcheck // best-effort drain
				for range remaining {
					if lr := <-ch; lr.cancel != nil {
						lr.cancel()
					}
				}
			}()
		}

		o.cache.Set(key, r.name)
		o.recordOperation(operation, r.name, start, nil)
		span.SetAttributes(telemetry.AttrBackendName.String(r.name))
		span.SetAttributes(telemetry.AttrObjectSize.Int64(r.size))
		span.SetAttributes(telemetry.AttrParallelBroadcast.Bool(true))
		span.SetStatus(codes.Ok, "")
		return r.name, nil
	}

	if lastErr != nil {
		span.SetStatus(codes.Error, lastErr.Error())
		span.RecordError(lastErr)
		return "", fmt.Errorf("all backends failed during degraded read: %w", lastErr)
	}

	span.SetStatus(codes.Error, "no backends available")
	return "", store.ErrObjectNotFound
}

// -------------------------------------------------------------------------
// READ OPERATIONS
// -------------------------------------------------------------------------

// GetObject retrieves an object from the backend where it's stored. Tries the
// primary copy first, then falls back to replicas if the primary fails. When
// the object is encrypted, the response body is transparently decrypted and
// the reported size reflects the original plaintext size.
func (o *ObjectManager) GetObject(ctx context.Context, key string, rangeHeader string) (*s3be.GetObjectResult, error) {
	// Check object data cache for full reads (non-range requests).
	if o.objectCache != nil && rangeHeader == "" {
		if entry, ok := o.objectCache.Get(key); ok {
			audit.Log(ctx, "storage.GetObject",
				slog.String("key", key),
				slog.String("backend", "cache"),
				slog.Int64("size", int64(len(entry.Data))),
			)
			return &s3be.GetObjectResult{
				Body:        io.NopCloser(bytes.NewReader(entry.Data)),
				Size:        int64(len(entry.Data)),
				ContentType: entry.ContentType,
				ETag:        entry.ETag,
				Metadata:    entry.Metadata,
			}, nil
		}
	}

	var result *s3be.GetObjectResult
	var once sync.Once // protects result write when parallel broadcast is enabled

	// Resolve locations upfront so encryption metadata is available after read.
	locations, locErr := o.store.GetAllObjectLocations(ctx, key)

	// Build a map for O(1) location lookup per backend attempt.
	var locByBackend map[string]*store.ObjectLocation
	if o.encryptor != nil && locErr == nil {
		locByBackend = make(map[string]*store.ObjectLocation, len(locations))
		for i := range locations {
			locByBackend[locations[i].BackendName] = &locations[i]
		}
	}

	backendName, err := o.withReadFailover(ctx, "GetObject", key, func(ctx context.Context, beName string, backend s3be.ObjectBackend) (int64, error) {
		if !o.usage.WithinLimits(beName, 1, 0, 0) {
			return 0, fmt.Errorf("backend %s: %w", beName, errUsageLimitSkip)
		}

		// Reject encrypted object reads when the DB is unavailable.
		// Without encryption metadata (wrapped DEK, key ID), the response
		// would contain raw ciphertext instead of plaintext.
		if o.encryptor != nil && locByBackend == nil {
			return 0, store.ErrServiceUnavailable
		}

		// Find encryption metadata for this backend's copy
		loc := locByBackend[beName]

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

		// Wrap with integrity verification if enabled
		if icfg := o.integrityCfg(); icfg != nil && icfg.Enabled && icfg.VerifyOnRead {
			expectedHash := ""
			if loc != nil {
				expectedHash = loc.ContentHash
			}
			if expectedHash != "" {
				vr := NewVerifyingReader(r.Body)
				vr.SetVerification(expectedHash, func(expected, actual string) {
					slog.ErrorContext(ctx, "Integrity check failed on read",
						"key", key, "backend", beName,
						"expected_hash", expected, "actual_hash", actual)
					telemetry.IntegrityErrorsTotal.WithLabelValues("read").Inc()
					o.deleteOrEnqueue(ctx, backend, beName, key, "integrity_failed", r.Size)
				})
				r.Body = vr
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

	// Populate object data cache on full reads. Read the response body into
	// memory, cache it, and replace the body with a bytes.Reader. Objects
	// that exceed max_object_size are silently skipped by the cache.
	if o.objectCache != nil && rangeHeader == "" {
		data, readErr := io.ReadAll(result.Body)
		result.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read object for caching: %w", readErr)
		}
		_ = o.objectCache.Put(key, bytes.NewReader(data), objcache.EntryMeta{
			ContentType: result.ContentType,
			ETag:        result.ETag,
			Metadata:    result.Metadata,
		})
		result.Body = io.NopCloser(bytes.NewReader(data))
	}

	return result, nil
}

// HeadObject retrieves object metadata. Tries the primary copy first, then
// falls back to replicas if the primary fails. When the object is encrypted,
// the reported size reflects the original plaintext size.
func (o *ObjectManager) HeadObject(ctx context.Context, key string) (*s3be.HeadObjectResult, error) {
	var result *s3be.HeadObjectResult
	var once sync.Once // protects result write when parallel broadcast is enabled

	// Resolve locations upfront so encryption metadata is available.
	locations, locErr := o.store.GetAllObjectLocations(ctx, key)

	// Build a map for O(1) location lookup per backend attempt.
	var locByBackend map[string]*store.ObjectLocation
	if o.encryptor != nil && locErr == nil {
		locByBackend = make(map[string]*store.ObjectLocation, len(locations))
		for i := range locations {
			locByBackend[locations[i].BackendName] = &locations[i]
		}
	}

	backendName, err := o.withReadFailover(ctx, "HeadObject", key, func(ctx context.Context, beName string, backend s3be.ObjectBackend) (int64, error) {
		if !o.usage.WithinLimits(beName, 1, 0, 0) {
			return 0, fmt.Errorf("backend %s: %w", beName, errUsageLimitSkip)
		}
		r, err := backend.HeadObject(ctx, key)
		if err != nil {
			o.usage.Record(beName, 1, 0, 0) // API call was made even on failure
			return 0, err
		}

		// Return plaintext size for encrypted objects
		if loc := locByBackend[beName]; loc != nil && loc.Encrypted {
			r.Size = loc.PlaintextSize
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

// ListObjectsV2Result holds the processed result for the S3 ListObjectsV2 response.
type ListObjectsV2Result struct {
	Objects               []store.ObjectLocation
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
		attribute.String("s3o.prefix", prefix),
		attribute.String("s3o.delimiter", delimiter),
		attribute.Int("s3o.max_keys", maxKeys),
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
			if errors.Is(err, store.ErrDBUnavailable) {
				span.SetStatus(codes.Error, "database unavailable")
				return nil, &store.S3Error{StatusCode: 503, Code: "ServiceUnavailable", Message: "listing unavailable during database outage"}
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
		for oi := range storeResult.Objects {
			// Check CommonPrefix membership before the limit check so
			// objects that collapse into an already-counted prefix are
			// skipped without triggering premature truncation.
			if delimiter != "" {
				rest := storeResult.Objects[oi].ObjectKey[len(prefix):]
				idx := strings.Index(rest, delimiter)
				if idx >= 0 {
					cp := storeResult.Objects[oi].ObjectKey[:len(prefix)+idx+len(delimiter)]
					if seen[cp] {
						// Same prefix already counted — skip silently
						lastKey = storeResult.Objects[oi].ObjectKey
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
					lastKey = storeResult.Objects[oi].ObjectKey
					continue
				}
			}

			// Regular object — enforce limit before adding
			if result.KeyCount >= maxKeys {
				result.IsTruncated = true
				result.NextContinuationToken = lastKey
				break
			}

			result.Objects = append(result.Objects, storeResult.Objects[oi])
			result.KeyCount++
			lastKey = storeResult.Objects[oi].ObjectKey
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
	span.SetAttributes(attribute.Int("s3o.key_count", result.KeyCount))
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
		start = max(plaintextSize-n, 0)
		return start, plaintextSize - 1, true
	}

	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}

	if parts[1] == "" {
		// Open-ended: bytes=N- (reject if start is beyond the file)
		if start >= plaintextSize {
			return 0, 0, false
		}
		return start, plaintextSize - 1, true
	}

	end, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}

	// Reject inverted ranges per RFC 7233 (last-byte-pos >= first-byte-pos)
	// and ranges that start beyond the file.
	if end < start || start >= plaintextSize {
		return 0, 0, false
	}

	// Clamp end to the last valid byte offset to prevent CiphertextRange
	// from requesting chunks beyond the actual object.
	if end >= plaintextSize {
		end = plaintextSize - 1
	}

	return start, end, true
}

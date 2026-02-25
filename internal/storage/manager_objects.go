// -------------------------------------------------------------------------------
// Object Operations - PUT, GET, HEAD, DELETE, COPY
//
// Author: Alex Freidah
//
// Object-level CRUD operations on the BackendManager. Handles backend selection
// via routing strategy, read failover across replicas, broadcast reads during
// degraded mode, and usage limit enforcement on reads and writes.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// -------------------------------------------------------------------------
// OBJECT CRUD
// -------------------------------------------------------------------------

// PutObject uploads an object to the first backend with available quota.
func (m *BackendManager) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) (string, error) {
	const operation = "PutObject"
	start := time.Now()

	// --- Start tracing span ---
	ctx, span := telemetry.StartSpan(ctx, "Manager "+operation,
		telemetry.AttrObjectKey.String(key),
		telemetry.AttrObjectSize.Int64(size),
	)
	defer span.End()

	// --- Filter backends within usage limits ---
	eligible := m.usage.BackendsWithinLimits(m.order,1, 0, size)
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

	// --- Upload to backend ---
	bctx, bcancel := m.withTimeout(ctx)
	defer bcancel()
	etag, err := backend.PutObject(bctx, key, body, size, contentType)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return "", err
	}

	// --- Record object location and update quota ---
	if err := m.recordObjectOrCleanup(ctx, span, backend, key, backendName, size); err != nil {
		return "", err
	}

	// --- Invalidate location cache ---
	m.cache.Delete(key)

	// --- Record metrics ---
	m.recordOperation(operation, backendName, start, nil)
	m.usage.Record(backendName, 1, 0, size)

	span.SetStatus(codes.Ok, "")
	return etag, nil
}

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
		bcancel()
		if err != nil {
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
// location cache first for a known-good backend, then falls back to trying
// every backend in order.
func (m *BackendManager) broadcastRead(ctx context.Context, operation, key string, start time.Time, span trace.Span, tryBackend func(ctx context.Context, backendName string, backend ObjectBackend) (int64, error)) (string, error) {
	span.SetAttributes(attribute.Bool("s3proxy.degraded_mode", true))
	telemetry.DegradedReadsTotal.WithLabelValues(operation).Inc()

	// --- Check location cache first ---
	if cachedBackend, ok := m.cache.Get(key); ok {
		if backend, exists := m.backends[cachedBackend]; exists {
			bctx, bcancel := m.withTimeout(ctx)
			size, err := tryBackend(bctx, cachedBackend, backend)
			bcancel()
			if err == nil {
				m.recordOperation(operation, cachedBackend, start, nil)
				span.SetAttributes(attribute.Bool("s3proxy.cache_hit", true))
				span.SetAttributes(telemetry.AttrObjectSize.Int64(size))
				span.SetStatus(codes.Ok, "")
				telemetry.DegradedCacheHitsTotal.Inc()
				return cachedBackend, nil
			}
			// Cache hit but backend failed — fall through to broadcast
		}
	}

	// --- Broadcast to all backends ---
	var lastErr error
	for _, name := range m.order {
		backend, ok := m.backends[name]
		if !ok {
			continue
		}
		bctx, bcancel := m.withTimeout(ctx)
		size, err := tryBackend(bctx, name, backend)
		bcancel()
		if err != nil {
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

// GetObject retrieves an object from the backend where it's stored. Tries the
// primary copy first, then falls back to replicas if the primary fails.
func (m *BackendManager) GetObject(ctx context.Context, key string, rangeHeader string) (*GetObjectResult, error) {
	var result *GetObjectResult

	backendName, err := m.withReadFailover(ctx, "GetObject", key, func(ctx context.Context, beName string, backend ObjectBackend) (int64, error) {
		if !m.usage.WithinLimits(beName, 1, 0, 0) {
			return 0, fmt.Errorf("backend %s: %w", beName, errUsageLimitSkip)
		}
		r, err := backend.GetObject(ctx, key, rangeHeader)
		if err != nil {
			return 0, err
		}
		if !m.usage.WithinLimits(beName, 1, r.Size, 0) {
			_ = r.Body.Close()
			return 0, fmt.Errorf("backend %s egress: %w", beName, errUsageLimitSkip)
		}
		result = r
		return r.Size, nil
	})
	if err != nil {
		return nil, err
	}
	m.usage.Record(backendName, 1, result.Size, 0)
	return result, nil
}

// HeadObject retrieves object metadata. Tries the primary copy first, then
// falls back to replicas if the primary fails.
func (m *BackendManager) HeadObject(ctx context.Context, key string) (int64, string, string, error) {
	var (
		rSize        int64
		rContentType string
		rETag        string
	)

	backendName, err := m.withReadFailover(ctx, "HeadObject", key, func(ctx context.Context, beName string, backend ObjectBackend) (int64, error) {
		if !m.usage.WithinLimits(beName, 1, 0, 0) {
			return 0, fmt.Errorf("backend %s: %w", beName, errUsageLimitSkip)
		}
		size, contentType, etag, err := backend.HeadObject(ctx, key)
		if err != nil {
			return 0, err
		}
		rSize, rContentType, rETag = size, contentType, etag
		return size, nil
	})
	if err != nil {
		return 0, "", "", err
	}
	m.usage.Record(backendName, 1, 0, 0)
	return rSize, rContentType, rETag, nil
}

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
	var srcFound bool
	for _, loc := range locations {
		if !m.usage.WithinLimits(loc.BackendName, 1, 0, 0) {
			continue
		}
		backend, ok := m.backends[loc.BackendName]
		if !ok {
			continue
		}
		bctx, bcancel := m.withTimeout(ctx)
		s, ct, _, err := backend.HeadObject(bctx, sourceKey)
		bcancel()
		if err != nil {
			continue
		}
		size = s
		contentType = ct
		srcFound = true
		break
	}
	if !srcFound {
		err := fmt.Errorf("failed to head source object from any copy")
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	span.SetAttributes(telemetry.AttrObjectSize.Int64(size))

	// --- Find destination backend with available quota and usage limits ---
	destEligible := m.usage.BackendsWithinLimits(m.order,1, 0, size)
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
	etag, err := destBackend.PutObject(ctx, destKey, pr, size, contentType)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return "", fmt.Errorf("failed to write destination: %w", err)
	}

	// --- Record destination location and update quota ---
	if err := m.recordObjectOrCleanup(ctx, span, destBackend, destKey, destBackendName, size); err != nil {
		return "", err
	}

	// --- Invalidate location cache ---
	m.cache.Delete(destKey)

	m.recordOperation(operation, destBackendName, start, nil)
	if srcName, ok := <-srcBackendCh; ok {
		m.usage.Record(srcName, 1, size, 0) // source: Get + egress
	}
	m.usage.Record(destBackendName, 1, 0, size) // dest: Put + ingress

	span.SetStatus(codes.Ok, "")
	return etag, nil
}

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
		bctx, bcancel := m.withTimeout(ctx)
		err := backend.DeleteObject(bctx, key)
		bcancel()
		if err != nil {
			// Log error but don't fail - quota is already updated
			slog.Warn("Failed to delete object from backend",
				"backend", copy.BackendName, "key", key, "error", err)
			span.RecordError(err)
		}
	}

	// --- Record metrics (use first copy's backend for primary) ---
	if len(copies) > 0 {
		m.recordOperation(operation, copies[0].BackendName, start, nil)
	}
	for _, c := range copies {
		m.usage.Record(c.BackendName, 1, 0, 0)
	}

	span.SetStatus(codes.Ok, "")
	return nil
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

		for _, obj := range storeResult.Objects {
			if result.KeyCount >= maxKeys {
				result.IsTruncated = true
				result.NextContinuationToken = obj.ObjectKey
				break
			}

			if delimiter != "" {
				rest := obj.ObjectKey[len(prefix):]
				idx := strings.Index(rest, delimiter)
				if idx >= 0 {
					cp := obj.ObjectKey[:len(prefix)+idx+len(delimiter)]
					if !seen[cp] {
						seen[cp] = true
						result.CommonPrefixes = append(result.CommonPrefixes, cp)
						result.KeyCount++
					}
					continue
				}
			}

			result.Objects = append(result.Objects, obj)
			result.KeyCount++
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

	m.recordOperation(operation, "", start, nil)
	span.SetStatus(codes.Ok, "")
	span.SetAttributes(attribute.Int("s3.key_count", result.KeyCount))
	return result, nil
}

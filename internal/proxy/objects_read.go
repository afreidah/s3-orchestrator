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

// resolveLocationsByBackend looks up every copy of key and returns a
// backend-name → location map for O(1) lookups inside the failover
// closure. Returns nil when encryption is disabled (the map is never
// consulted) or when the DB lookup failed. Callers that care about the
// DB-down case inspect o.encryptor alongside the returned map — when
// o.encryptor != nil and the map is nil, the database failed.
func (o *ObjectManager) resolveLocationsByBackend(ctx context.Context, key string) map[string]*store.ObjectLocation {
	if o.encryptor == nil {
		return nil
	}
	locations, err := o.objects.GetAllObjectLocations(ctx, key)
	if err != nil {
		return nil
	}
	m := make(map[string]*store.ObjectLocation, len(locations))
	for i := range locations {
		m[locations[i].BackendName] = &locations[i]
	}
	return m
}

// readBackendFn is the per-backend probe callback. The callback owns its own
// timeout context and is responsible for releasing it on the error path. On
// success it returns a cleanup func the orchestrator invokes once the winner
// is declared; that cleanup either releases the timeout immediately
// (HeadObject — no streaming body) or is a no-op because the callback has
// already attached the cancel to the result body's Close (GetObject).
type readBackendFn func(ctx context.Context, backendName string, backend s3be.ObjectBackend) (int64, func(), error)

// noopCleanup is the cleanup returned by readBackendFn implementations
// when there is nothing for the orchestrator to release: error paths
// where the callback already cancelled its own timeout, and the GetObject
// success path where the cancel is attached to the body's Close.
func noopCleanup() {
	// meant to be a noop function
}

// withReadFailover looks up all copies of an object and tries each backend in
// order until one succeeds. The tryBackend callback should attempt the operation
// and return the object size (for span attributes) or an error to try the next copy.
// Returns the name of the backend that succeeded alongside any error.
func (o *ObjectManager) withReadFailover(ctx context.Context, operation, key string, tryBackend readBackendFn) (string, error) {
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
		telemetry.AttrObjectKey.String(key),
	)
	defer span.End()

	locations, err := o.objects.GetAllObjectLocations(ctx, key)
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

	return o.tryEachLocation(ctx, operation, key, start, span, locations, tryBackend)
}

// tryEachLocation walks the resolved location list and runs the per-backend
// callback against each, returning on the first success. Records lastErr
// and a count of usage-limit skips so the failure-path return distinguishes
// "all backends declined for usage limits" from "all backends genuinely
// failed."
func (o *ObjectManager) tryEachLocation(ctx context.Context, operation, key string, start time.Time, span trace.Span, locations []store.ObjectLocation, tryBackend readBackendFn) (string, error) {
	var lastErr error
	var limitSkips int
	for i := range locations {
		span.SetAttributes(telemetry.AttrBackendName.String(locations[i].BackendName))
		name := locations[i].BackendName

		backend, ok := o.backends[name]
		if !ok {
			lastErr = fmt.Errorf("backend %s not found", name)
			continue
		}

		size, cleanup, err := tryBackend(ctx, name, backend)
		if err != nil {
			lastErr = err
			if errors.Is(err, errUsageLimitSkip) {
				limitSkips++
			}
			if i < len(locations)-1 {
				slog.WarnContext(ctx, operation+": copy failed, trying next",
					"key", key, "failed_backend", name, "error", err)
			}
			continue
		}

		cleanup()
		o.recordOperation(operation, name, start, nil)
		if i > 0 {
			span.SetAttributes(telemetry.AttrFailover.Bool(true))
		}
		span.SetAttributes(telemetry.AttrObjectSize.Int64(size))
		span.SetStatus(codes.Ok, "")
		return name, nil
	}

	return "", failoverFailureResult(span, operation, locations, lastErr, limitSkips)
}

// failoverFailureResult finalises the span and chooses between the
// usage-limit-exceeded sentinel and the underlying lastErr based on
// whether every location declined for over-limit reasons.
func failoverFailureResult(span trace.Span, operation string, locations []store.ObjectLocation, lastErr error, limitSkips int) error {
	if limitSkips > 0 && limitSkips == len(locations) {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "read").Inc()
		span.SetStatus(codes.Error, "all copies over usage limit")
		return store.ErrUsageLimitExceeded
	}
	span.SetStatus(codes.Error, lastErr.Error())
	span.RecordError(lastErr)
	return lastErr
}

// broadcastRead tries all backends when the DB is unavailable. Checks the
// location cache first for a known-good backend, then dispatches to either
// parallel or sequential broadcast based on configuration.
func (o *ObjectManager) broadcastRead(ctx context.Context, operation, key string, start time.Time, span trace.Span, tryBackend readBackendFn) (string, error) {
	span.SetAttributes(telemetry.AttrDegradedMode.Bool(true))
	telemetry.DegradedReadsTotal.WithLabelValues(operation).Inc()

	// --- Check location cache first ---
	if cachedBackend, ok := o.cache.Get(key); ok {
		if backend, exists := o.backends[cachedBackend]; exists {
			size, cleanup, err := tryBackend(ctx, cachedBackend, backend)
			if err == nil {
				cleanup()
				o.recordOperation(operation, cachedBackend, start, nil)
				span.SetAttributes(telemetry.AttrCacheHit.Bool(true))
				span.SetAttributes(telemetry.AttrObjectSize.Int64(size))
				span.SetStatus(codes.Ok, "")
				telemetry.DegradedCacheHitsTotal.Inc()
				return cachedBackend, nil
			}
			// Cache hit but backend failed — fall through to broadcast.
			// The callback already released its timeout on the error path.
		}
	}

	concurrency := 1
	if o.parallelBroadcast {
		concurrency = len(o.order)
	}
	return o.tryAllBackends(ctx, operation, key, start, span, concurrency, tryBackend)
}

// tryAllBackends dispatches to the sequential or parallel branch based on
// concurrency. Both branches return the first backend whose tryBackend call
// succeeds and cache the winner's name for future degraded reads.
func (o *ObjectManager) tryAllBackends(
	ctx context.Context,
	operation, key string,
	start time.Time,
	span trace.Span,
	concurrency int,
	tryBackend readBackendFn,
) (string, error) {
	if concurrency <= 1 {
		return o.tryBackendsSequentially(ctx, operation, key, start, span, tryBackend)
	}
	return o.tryBackendsInParallel(ctx, operation, key, start, span, tryBackend)
}

// tryBackendsSequentially walks o.order one backend at a time. The first
// success short-circuits and is recorded as the broadcast winner; otherwise
// the last error (if any) is wrapped as a degraded-read failure.
func (o *ObjectManager) tryBackendsSequentially(
	ctx context.Context,
	operation, key string,
	start time.Time,
	span trace.Span,
	tryBackend readBackendFn,
) (string, error) {
	var lastErr error
	for _, name := range o.order {
		backend, ok := o.backends[name]
		if !ok {
			continue
		}
		size, cleanup, err := tryBackend(ctx, name, backend)
		if err != nil {
			lastErr = err
			continue
		}
		cleanup()
		o.recordBroadcastWinner(operation, key, name, size, start, span, false)
		return name, nil
	}
	return broadcastAllFailed(span, lastErr)
}

// broadcastResult carries one parallel-probe outcome back to the
// fan-in loop. cleanup is the success-path cleanup the orchestrator
// invokes either inline (winner) or via the loser-drain goroutine.
type broadcastResult struct {
	name    string
	size    int64
	err     error
	cleanup func()
}

// tryBackendsInParallel launches one probe per backend in o.order and
// returns the first success, draining the remaining responses in the
// background so loser cleanups fire promptly.
func (o *ObjectManager) tryBackendsInParallel(
	ctx context.Context,
	operation, key string,
	start time.Time,
	span trace.Span,
	tryBackend readBackendFn,
) (string, error) {
	ch := make(chan broadcastResult, len(o.order))
	launched := o.launchBackendProbes(ctx, ch, tryBackend)

	var lastErr error
	for received := 0; received < launched; received++ { //nolint:intrange // received used in arithmetic below
		r := <-ch
		if r.err != nil {
			lastErr = r.err
			continue
		}
		if r.cleanup != nil {
			r.cleanup()
		}
		if remaining := launched - received - 1; remaining > 0 {
			go drainAndCleanupLosers(ch, remaining)
		}
		o.recordBroadcastWinner(operation, key, r.name, r.size, start, span, true)
		return r.name, nil
	}
	return broadcastAllFailed(span, lastErr)
}

// launchBackendProbes spawns one goroutine per eligible backend, each of
// which writes its outcome to ch. Returns the number of probes launched
// so the caller knows how many results to read.
func (o *ObjectManager) launchBackendProbes(
	ctx context.Context,
	ch chan<- broadcastResult,
	tryBackend readBackendFn,
) int {
	launched := 0
	for _, name := range o.order {
		backend, ok := o.backends[name]
		if !ok {
			continue
		}
		launched++
		go o.runBackendProbe(ctx, name, backend, tryBackend, ch)
	}
	return launched
}

// runBackendProbe is the per-backend goroutine body. On success it
// forwards the callback's cleanup so the orchestrator (winner) or the
// loser-drain (everyone else) can release the timeout context promptly
// instead of waiting for the deadline to fire on its own.
func (o *ObjectManager) runBackendProbe(
	ctx context.Context,
	name string,
	backend s3be.ObjectBackend,
	tryBackend readBackendFn,
	ch chan<- broadcastResult,
) {
	size, cleanup, err := tryBackend(ctx, name, backend)
	if err != nil {
		ch <- broadcastResult{name: name, err: err}
		return
	}
	ch <- broadcastResult{name: name, size: size, cleanup: cleanup}
}

// drainAndCleanupLosers reads the remaining results from ch after a winner
// has been declared and invokes any cleanup the losers returned. Best-effort:
// a sender that panicked or closed early is tolerated.
func drainAndCleanupLosers(ch <-chan broadcastResult, remaining int) {
	defer func() { recover() }() //nolint:errcheck // best-effort drain
	for range remaining {
		if lr := <-ch; lr.cleanup != nil {
			lr.cleanup()
		}
	}
}

// recordBroadcastWinner caches the winner's name for future degraded reads,
// emits operation metrics, and sets the success-path span attributes.
func (o *ObjectManager) recordBroadcastWinner(
	operation, key, name string,
	size int64,
	start time.Time,
	span trace.Span,
	parallel bool,
) {
	o.cache.Set(key, name)
	o.recordOperation(operation, name, start, nil)
	span.SetAttributes(telemetry.AttrBackendName.String(name))
	span.SetAttributes(telemetry.AttrObjectSize.Int64(size))
	if parallel {
		span.SetAttributes(telemetry.AttrParallelBroadcast.Bool(true))
	}
	span.SetStatus(codes.Ok, "")
}

// broadcastAllFailed builds the all-failed return value. When at least one
// backend returned an error, that error is wrapped so the server can
// distinguish "backend unreachable" (502) from "object not found" (404).
func broadcastAllFailed(span trace.Span, lastErr error) (string, error) {
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
	locByBackend := o.resolveLocationsByBackend(ctx, key)

	backendName, err := o.withReadFailover(ctx, "GetObject", key, func(ctx context.Context, beName string, backend s3be.ObjectBackend) (int64, func(), error) {
		// Per-callback timeout context. The callback owns bcancel on every
		// path: error paths and once-loser branches release immediately;
		// the once-winner attaches bcancel to the body's Close so the
		// streaming read keeps the context alive until the consumer is done.
		bctx, bcancel := o.withTimeout(ctx)

		if !o.usage.WithinLimits(beName, 1, 0, 0) {
			bcancel()
			return 0, noopCleanup, fmt.Errorf("backend %s: %w", beName, errUsageLimitSkip)
		}

		// Reject encrypted object reads when the DB is unavailable.
		// Without encryption metadata (wrapped DEK, key ID), the response
		// would contain raw ciphertext instead of plaintext.
		if o.encryptor != nil && locByBackend == nil {
			bcancel()
			return 0, noopCleanup, store.ErrServiceUnavailable
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

		r, err := backend.GetObject(bctx, key, actualRange)
		if err != nil {
			bcancel()
			o.usage.Record(beName, 1, 0, 0)
			return 0, noopCleanup, err
		}
		if !o.usage.WithinLimits(beName, 1, r.Size, 0) {
			_ = r.Body.Close()
			bcancel()
			o.usage.Record(beName, 1, 0, 0)
			return 0, noopCleanup, fmt.Errorf("backend %s egress: %w", beName, errUsageLimitSkip)
		}

		// Decrypt the response body if the object is encrypted
		if loc != nil && loc.Encrypted && o.encryptor != nil {
			if err := decryptResponse(ctx, o.encryptor, r, loc, rng, ptStart, ptEnd); err != nil {
				_ = r.Body.Close()
				bcancel()
				return 0, noopCleanup, err
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
					slog.ErrorContext(ctx, "integrity check failed on read",
						"key", key, "backend", beName,
						"expected_hash", expected, "actual_hash", actual)
					telemetry.IntegrityErrorsTotal.WithLabelValues("read").Inc()
					o.deleteOrEnqueue(ctx, backend, beName, key, "integrity_failed", r.Size)
				})
				r.Body = vr
			}
		}

		// Wrap the body's Close so the per-callback timeout is released
		// when the consumer finishes reading. Done before once.Do so the
		// winning result carries the wrapped Close; losers throw it away.
		r.Body = bodyWithCancel(r.Body, bcancel)

		once.Do(func() { result = r })
		if result != r {
			// Loser: close our body which fires bcancel via the wrapper.
			_ = r.Body.Close()
		}
		return r.Size, noopCleanup, nil
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
	locByBackend := o.resolveLocationsByBackend(ctx, key)

	backendName, err := o.withReadFailover(ctx, "HeadObject", key, func(ctx context.Context, beName string, backend s3be.ObjectBackend) (int64, func(), error) {
		bctx, bcancel := o.withTimeout(ctx)
		if !o.usage.WithinLimits(beName, 1, 0, 0) {
			bcancel()
			return 0, noopCleanup, fmt.Errorf("backend %s: %w", beName, errUsageLimitSkip)
		}
		r, err := backend.HeadObject(bctx, key)
		if err != nil {
			bcancel()
			o.usage.Record(beName, 1, 0, 0) // API call was made even on failure
			return 0, noopCleanup, err
		}

		// Return plaintext size for encrypted objects
		if loc := locByBackend[beName]; loc != nil && loc.Encrypted {
			r.Size = loc.PlaintextSize
		}

		once.Do(func() { result = r })
		// HEAD has no streaming body to keep alive; the orchestrator (winner)
		// or the loser-drain invokes the returned cleanup to release the
		// timeout immediately rather than waiting for the deadline.
		return r.Size, bcancel, nil
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

// advancePastEmittedCommonPrefix rewrites a continuation cursor so the
// next ListObjects call cannot re-emit a CommonPrefix the current call
// already returned. The seen map is local to a single ListObjects
// invocation, so without this rewrite a cursor that lands inside an
// already-emitted CP (e.g., maxPages cap reached deep in a tenant's keys
// or the page boundary aligned mid-group) would let the next call walk
// the same group and emit its CP a second time.
//
// The rewrite increments the last byte of the CP, producing the smallest
// string lex-greater than every key starting with that CP. The store's
// next-page WHERE object_key > cursor then skips the rest of the group
// cleanly. Returns the input unchanged when the delimiter is unset, the
// cursor does not fall inside an emitted CP, or the last byte is 0xff
// (no representable advance — accept potential re-emission rather than
// corrupt the cursor).
func advancePastEmittedCommonPrefix(prefix, delimiter, cursor string, seen map[string]bool) string {
	if delimiter == "" || cursor == "" {
		return cursor
	}
	if !strings.HasPrefix(cursor, prefix) {
		return cursor
	}
	rest := cursor[len(prefix):]
	idx := strings.Index(rest, delimiter)
	if idx < 0 {
		return cursor
	}
	cp := cursor[:len(prefix)+idx+len(delimiter)]
	if !seen[cp] {
		return cursor
	}
	last := cp[len(cp)-1]
	if last == 0xff {
		return cursor
	}
	return cp[:len(cp)-1] + string([]byte{last + 1})
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

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
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
		storeResult, err := o.objects.ListObjects(ctx, prefix, cursor, maxKeys)
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
			result.NextContinuationToken = advancePastEmittedCommonPrefix(prefix, delimiter, cursor, seen)
		}
	}

	// When we consumed exactly maxKeys entries from a single store page
	// and the store reported more data, the inner loop never tried to add
	// a (maxKeys+1)th entry so IsTruncated was never set. Detect this and
	// mark the result as truncated so the client paginates correctly.
	if !result.IsTruncated && lastStoreTruncated && result.KeyCount >= maxKeys {
		result.IsTruncated = true
		result.NextContinuationToken = advancePastEmittedCommonPrefix(prefix, delimiter, cursor, seen)
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

	// Reject ranges whose first-byte-pos is beyond the file. Applies to both
	// open-ended (bytes=N-) and explicit (bytes=N-M) forms.
	if start >= plaintextSize {
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

	// Reject inverted ranges per RFC 7233 (last-byte-pos >= first-byte-pos).
	if end < start {
		return 0, 0, false
	}

	// Clamp end to the last valid byte offset to prevent CiphertextRange
	// from requesting chunks beyond the actual object.
	if end >= plaintextSize {
		end = plaintextSize - 1
	}

	return start, end, true
}

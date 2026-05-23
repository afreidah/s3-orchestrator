// -------------------------------------------------------------------------------
// Object Manager - GET
//
// Author: Alex Freidah
//
// GetObject orchestration: object-data cache fast path, per-backend attempt
// callback driven by readpath.Failover, encrypted-range translation,
// optional VerifyingReader wrapping for read-time integrity, and
// streaming cache population on a clean read. The cache-tee body
// implementation lives in cache.go; the verifying reader in
// integrity_reader.go.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	pobserve "github.com/afreidah/s3-orchestrator/internal/proxy/observe"
	"github.com/afreidah/s3-orchestrator/internal/proxy/readpath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/ioutilx"
)

// GetObject retrieves an object from the backend where it's stored. Tries
// the primary copy first, then falls back to replicas if the primary
// fails. When the object is encrypted, the response body is transparently
// decrypted and the reported size reflects the original plaintext size.
func (o *Manager) GetObject(ctx context.Context, key string, rangeHeader string) (*s3be.GetObjectResult, error) {
	if cached, ok := o.tryGetObjectCache(ctx, key, rangeHeader); ok {
		return cached, nil
	}

	var result *s3be.GetObjectResult
	var once sync.Once

	backendName, err := o.failover.Read(ctx, "GetObject", key, func(ctx context.Context, beName string, loc *core.ObjectLocation, backend s3be.ObjectBackend) (int64, func(), error) {
		return o.getObjectAttempt(ctx, key, rangeHeader, beName, backend, loc, &once, &result)
	})
	if err != nil {
		return nil, err
	}
	o.core.Acct().Egress(backendName, result.Size)

	pobserve.GetCompleted(ctx, key, backendName, result.Size)

	if err := o.populateObjectCache(key, rangeHeader, result); err != nil {
		return nil, err
	}
	return result, nil
}

// tryGetObjectCache returns a synthesized GetObjectResult when the object
// data cache holds a non-range hit for this key. ok=false signals that the
// caller must read from a backend.
func (o *Manager) tryGetObjectCache(ctx context.Context, key, rangeHeader string) (*s3be.GetObjectResult, bool) {
	if o.objectCache == nil || rangeHeader != "" {
		return nil, false
	}
	entry, ok := o.objectCache.Get(key)
	if !ok {
		return nil, false
	}
	pobserve.GetCompleted(ctx, key, "cache", int64(len(entry.Data)))
	return &s3be.GetObjectResult{
		Body:        io.NopCloser(bytes.NewReader(entry.Data)),
		Size:        int64(len(entry.Data)),
		ContentType: entry.ContentType,
		ETag:        entry.ETag,
		Metadata:    entry.Metadata,
	}, true
}

// getObjectAttempt is the per-backend callback invoked by withReadFailover
// for GetObject. It owns the per-attempt timeout, applies usage limits,
// translates encrypted ranges, decrypts and verifies the body, and records
// the winning result via once. loc is nil in degraded-mode broadcasts.
func (o *Manager) getObjectAttempt(ctx context.Context, key, rangeHeader, beName string, backend s3be.ObjectBackend, loc *core.ObjectLocation, once *sync.Once, result **s3be.GetObjectResult) (int64, func(), error) {
	bctx, bcancel := o.core.WithTimeout(ctx)

	if !o.core.Usage().WithinLimits(beName, 1, 0, 0) {
		bcancel()
		return 0, readpath.NoopCleanup, fmt.Errorf("backend %s: %w", beName, readpath.ErrUsageLimitSkip)
	}
	// Encrypted reads need the location row to unwrap the DEK; without it
	// (degraded broadcast with the DB unreachable) we cannot decrypt.
	if o.encryptor != nil && loc == nil {
		bcancel()
		return 0, readpath.NoopCleanup, core.ErrServiceUnavailable
	}

	actualRange, rng, ptStart, ptEnd := o.resolveBackendRange(rangeHeader, loc)

	r, err := backend.GetObject(bctx, key, actualRange)
	if err != nil {
		bcancel()
		o.core.Acct().APICall(beName)
		return 0, readpath.NoopCleanup, err
	}
	if !o.core.Usage().WithinLimits(beName, 1, r.Size, 0) {
		_ = r.Body.Close()
		bcancel()
		o.core.Acct().APICall(beName)
		return 0, readpath.NoopCleanup, fmt.Errorf("backend %s egress: %w", beName, readpath.ErrUsageLimitSkip)
	}

	if loc != nil && loc.Encrypted && o.encryptor != nil {
		if err := decryptResponse(ctx, o.encryptor, r, loc, rng, ptStart, ptEnd); err != nil {
			_ = r.Body.Close()
			bcancel()
			return 0, readpath.NoopCleanup, err
		}
	}

	o.maybeWrapIntegrityReader(ctx, r, loc, key, beName, backend)

	r.Body = ioutilx.WithCancel(r.Body, bcancel)
	once.Do(func() { *result = r })
	if *result != r {
		_ = r.Body.Close()
	}
	return r.Size, readpath.NoopCleanup, nil
}

// resolveBackendRange translates a plaintext Range header into the actual
// ciphertext range to request from the backend. Returns the original
// header verbatim for unencrypted objects.
func (o *Manager) resolveBackendRange(rangeHeader string, loc *core.ObjectLocation) (string, *encryption.RangeResult, int64, int64) {
	if loc == nil || !loc.Encrypted || rangeHeader == "" {
		return rangeHeader, nil, 0, 0
	}
	ptStart, ptEnd, ok := ParsePlaintextRange(rangeHeader, loc.PlaintextSize)
	if !ok {
		return rangeHeader, nil, 0, 0
	}
	rng, _ := encryption.CiphertextRange(ptStart, ptEnd, o.encryptor.ChunkSize())
	if rng == nil {
		return rangeHeader, nil, ptStart, ptEnd
	}
	return rng.BackendRange, rng, ptStart, ptEnd
}

// maybeWrapIntegrityReader replaces r.Body with a verifying reader when
// integrity verification is enabled and an expected content hash is
// available. A hash mismatch logs, increments telemetry, and enqueues the
// bad copy for cleanup.
func (o *Manager) maybeWrapIntegrityReader(
	ctx context.Context,
	r *s3be.GetObjectResult,
	loc *core.ObjectLocation,
	key, beName string,
	backend s3be.ObjectBackend,
) {
	icfg := o.integrityCfg.Load()
	if icfg == nil || !icfg.Enabled || !icfg.VerifyOnRead {
		return
	}
	expectedHash := ""
	if loc != nil {
		expectedHash = loc.ContentHash
	}
	if expectedHash == "" {
		return
	}
	vr := NewVerifyingReader(r.Body)
	vr.SetVerification(expectedHash, func(expected, actual string) {
		o.log.ErrorContext(ctx, "integrity check failed on read",
			"key", key, "backend", beName,
			"expected_hash", expected, "actual_hash", actual)
		telemetry.IntegrityErrorsTotal.WithLabelValues("read").Inc()
		o.coord.DeleteOrEnqueue(ctx, backend, beName, key, "integrity_failed", r.Size)
	})
	r.Body = vr
}

// populateObjectCache wraps result.Body in a tee that copies bytes into
// a pre-sized buffer as the response streams to the client. On clean
// read completion (EOF at exactly result.Size bytes) the buffer is
// handed to the cache. The cache is left untouched on early disconnect,
// mid-stream errors, or any short-/over-read versus the announced size.
//
// Skipped (no buffering, body unchanged) when:
//   - the cache is disabled
//   - the request carries a Range header (partial responses are not
//     stored as full-object cache entries)
//   - the backend did not return a positive Content-Length (size <= 0)
//   - the announced size exceeds the cache's per-entry admission limit
//
// In every skip case the backend body streams straight through to the
// client with zero proxy-side buffering, so a 5 GB GET with a 100 MB
// max_object_size never allocates more than the read buffer worth of
// heap.
func (o *Manager) populateObjectCache(key, rangeHeader string, result *s3be.GetObjectResult) error {
	if o.objectCache == nil || rangeHeader != "" || result.Size <= 0 {
		return nil
	}
	if !o.objectCache.Admit(result.Size) {
		return nil
	}
	meta := objcache.EntryMeta{
		ContentType: result.ContentType,
		ETag:        result.ETag,
		Metadata:    result.Metadata,
	}
	result.Body = newCacheTeeBody(result.Body, result.Size, func(data []byte) {
		o.objectCache.PutBytes(key, data, meta)
	})
	return nil
}

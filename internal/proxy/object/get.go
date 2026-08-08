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
	"strings"

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

	result, backendName, err := readpath.Read(ctx, o.failover, "GetObject", key,
		func(ctx context.Context, beName string, loc *core.ObjectLocation, backend s3be.ObjectBackend) (readpath.ProbeResult[*s3be.GetObjectResult], error) {
			return o.getObjectAttempt(ctx, key, rangeHeader, beName, backend, loc)
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
		Body:         io.NopCloser(bytes.NewReader(entry.Data)),
		Size:         int64(len(entry.Data)),
		ContentType:  entry.ContentType,
		ETag:         entry.ETag,
		LastModified: entry.LastModified,
		Metadata:     entry.Metadata,
	}, true
}

// getObjectAttempt is the per-backend callback invoked by withReadFailover
// for GetObject. It owns the per-attempt timeout, applies usage limits,
// translates encrypted ranges, decrypts and verifies the body, and records
// the winning result via once. loc is nil in degraded-mode broadcasts.
func (o *Manager) getObjectAttempt(ctx context.Context, key, rangeHeader, beName string, backend s3be.ObjectBackend, loc *core.ObjectLocation) (readpath.ProbeResult[*s3be.GetObjectResult], error) {
	var fail readpath.ProbeResult[*s3be.GetObjectResult]

	if !o.core.Usage().WithinLimits(beName, 1, 0, 0) {
		return fail, fmt.Errorf("backend %s: %w", beName, readpath.ErrUsageLimitSkip)
	}
	// Encrypted reads need the location row to unwrap the DEK; without it
	// (degraded broadcast with the DB unreachable) we cannot decrypt.
	if o.encryptor != nil && loc == nil {
		return fail, core.ErrServiceUnavailable
	}

	// Reject a copy whose row contradicts itself before doing range math on
	// its sizes. Failing here fails over to a sibling copy, so one damaged row
	// costs a retry rather than a wrong response body.
	if err := core.ValidateEncryptionMetadata(loc); err != nil {
		telemetry.EncryptionFlagMismatchTotal.WithLabelValues("get").Inc()
		return fail, fmt.Errorf("backend %s: %w", beName, err)
	}

	actualRange, rng, ptStart, ptEnd := o.resolveBackendRange(rangeHeader, loc)

	r, cancel, err := o.core.GetWithTimeout(ctx, backend, key, actualRange)
	if err != nil {
		o.core.Acct().APICall(beName)
		return fail, err
	}
	if !o.core.Usage().WithinLimits(beName, 1, r.Size, 0) {
		_ = r.Body.Close()
		cancel()
		o.core.Acct().APICall(beName)
		return fail, fmt.Errorf("backend %s egress: %w", beName, readpath.ErrUsageLimitSkip)
	}

	// Backends that report no modification time leave LastModified zero, which
	// the transport then drops from the response entirely. Fall back to the
	// stored creation time so every object carries a valid Last-Modified.
	if r.LastModified.IsZero() && loc != nil {
		r.LastModified = loc.CreatedAt
	}

	if err := verifyStoredEnvelope(r, loc, actualRange); err != nil {
		telemetry.EncryptionFlagMismatchTotal.WithLabelValues("get").Inc()
		_ = r.Body.Close()
		cancel()
		return fail, fmt.Errorf("backend %s: %w", beName, err)
	}

	if err := o.buildPlaintextReader(ctx, r, loc, key, beName, backend, rng, ptStart, ptEnd); err != nil {
		_ = r.Body.Close()
		cancel()
		return fail, err
	}

	// The body owns the timeout cancel via Close. The winner's body streams to
	// the client (cancel fires when the client closes it); a losing result is
	// released by Cleanup, which closes the body and so cancels the timeout.
	r.Body = ioutilx.WithCancel(r.Body, cancel)
	return readpath.ProbeResult[*s3be.GetObjectResult]{
		Value:   r,
		Size:    r.Size,
		Cleanup: func() { _ = r.Body.Close() },
	}, nil
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

// buildPlaintextReader turns a backend GetObject result into the client-facing
// plaintext stream: it decrypts in place when the object is encrypted, then
// wraps the body in a verifying reader when read-time integrity checking is
// enabled. This is the single read-path pipeline for both layers. Mutates
// r.Body/r.Size/r.ContentRange. The caller must close r.Body on error.
func (o *Manager) buildPlaintextReader(
	ctx context.Context,
	r *s3be.GetObjectResult,
	loc *core.ObjectLocation,
	key, beName string,
	backend s3be.ObjectBackend,
	rng *encryption.RangeResult,
	ptStart, ptEnd int64,
) error {
	if loc != nil && loc.Encrypted && o.encryptor != nil {
		if err := decryptResponse(ctx, o.encryptor, r, loc, rng, ptStart, ptEnd); err != nil {
			return err
		}
	}
	o.maybeWrapIntegrityReader(ctx, r, loc, key, beName, backend)
	return nil
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
		ContentType:  result.ContentType,
		ETag:         result.ETag,
		LastModified: result.LastModified,
		Metadata:     result.Metadata,
	}
	result.Body = newCacheTeeBody(result.Body, result.Size, func(data []byte) {
		o.objectCache.PutBytes(key, data, meta)
	})
	return nil
}

// verifyStoredEnvelope checks the bytes a backend actually returned against
// what the metadata row claims about them, and replaces r.Body with an
// equivalent stream so the caller reads from the start either way.
//
// The check only applies when the response begins at byte 0 of the stored
// object, which is where the envelope signature lives: a full read, or a
// range read of an object the row calls plaintext that starts at 0. A ranged
// read of an encrypted object starts past the header by construction, and a
// range that starts mid-object carries no signature to compare, so both are
// left to the scrubber to catch.
func verifyStoredEnvelope(r *s3be.GetObjectResult, loc *core.ObjectLocation, actualRange string) error {
	if loc == nil || (actualRange != "" && !strings.HasPrefix(actualRange, "bytes=0-")) {
		return nil
	}
	isEnvelope, body, err := encryption.PeekEnvelope(r.Body)
	if err != nil {
		return fmt.Errorf("inspect object header: %w", err)
	}
	r.Body = ioutilx.ReadCloser(body, r.Body)
	if isEnvelope != loc.Encrypted {
		return fmt.Errorf("%w: row says encrypted=%t but stored bytes say encrypted=%t",
			core.ErrEncryptionFlagMismatch, loc.Encrypted, isEnvelope)
	}
	return nil
}

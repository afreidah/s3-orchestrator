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
	"sync/atomic"

	"go.opentelemetry.io/otel/trace"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	pobserve "github.com/afreidah/s3-orchestrator/internal/proxy/observe"
	"github.com/afreidah/s3-orchestrator/internal/proxy/readpath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/ioutilx"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// GetObject retrieves an object from the backend where it's stored. Tries
// the primary copy first, then falls back to replicas if the primary
// fails. When the object is encrypted, the response body is transparently
// decrypted and the reported size reflects the original plaintext size.
func (o *Manager) GetObject(ctx context.Context, key string, rangeHeader string) (*GetResult, error) {
	if cached, ok := o.tryGetObjectCache(ctx, key, rangeHeader); ok {
		return cached, nil
	}

	// A compressed read meters itself, one charge per frame fetched, on the
	// bytes that actually left the backend. Every copy of a key agrees on
	// whether it is compressed, so whichever attempt wins reports the same
	// thing here.
	//
	// Every other read is charged here, on the bytes the winning attempt pulled
	// off the backend. That is not result.Size: decryption happens on this side
	// of the backend link, so an encrypted object is served as a smaller
	// plaintext than the ciphertext that crossed it, and a ranged read of one
	// crosses whole chunks to serve a slice.
	var selfMetered atomic.Bool
	var wireBytes atomic.Int64
	result, backendName, err := o.failover.Read(ctx, "GetObject", key,
		func(ctx context.Context, beName string, loc *core.ObjectLocation, backend s3be.ObjectBackend) (readpath.ProbeResult[*s3be.GetObjectResult], error) {
			if isCompressed(loc) {
				selfMetered.Store(true)
			}
			res, wire, err := o.getObjectAttempt(ctx, key, rangeHeader, beName, backend, loc)
			if err == nil {
				wireBytes.Store(wire)
				applyStoredIdentity(res.Value, loc)
			}
			return res, err
		})
	if err != nil {
		return nil, err
	}
	if !selfMetered.Load() {
		o.core.Acct().Egress(s3op.GetObject, backendName, wireBytes.Load())
	}

	pobserve.GetCompleted(ctx, key, backendName, result.Size)

	// Counted before the cache tee is attached so the entry this read populates
	// carries the count, and a later hit can answer without the store.
	tagCount := o.countObjectTags(ctx, key)
	if err := o.populateObjectCache(key, rangeHeader, result, tagCount); err != nil {
		return nil, err
	}
	return &GetResult{GetObjectResult: result, TagCount: tagCount}, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// tryGetObjectCache returns a synthesized GetObjectResult when the object
// data cache holds a non-range hit for this key. ok=false signals that the
// caller must read from a backend.
func (o *Manager) tryGetObjectCache(ctx context.Context, key, rangeHeader string) (*GetResult, bool) {
	if o.objectCache == nil || rangeHeader != "" {
		return nil, false
	}
	entry, ok := o.objectCache.Get(key)
	if !ok {
		return nil, false
	}
	pobserve.GetCompleted(ctx, key, "cache", int64(len(entry.Data)))
	return &GetResult{
		GetObjectResult: &s3be.GetObjectResult{
			Body:         io.NopCloser(bytes.NewReader(entry.Data)),
			Size:         int64(len(entry.Data)),
			ContentType:  entry.ContentType,
			ETag:         entry.ETag,
			LastModified: entry.LastModified,
			Metadata:     entry.Metadata,
		},
		TagCount: entry.TagCount,
	}, true
}

// getObjectAttempt is the per-backend callback invoked by withReadFailover
// for GetObject. It owns the per-attempt timeout, applies usage limits,
// translates encrypted ranges, decrypts and verifies the body, and records
// the winning result via once. loc is nil in degraded-mode broadcasts.
//
// The second return is how many bytes the backend served, which the caller
// charges as egress. It is read before the body is turned into plaintext,
// because that step rewrites the result's size to what the client will be
// given. Zero for a compressed copy, which meters itself per frame.
func (o *Manager) getObjectAttempt(ctx context.Context, key, rangeHeader, beName string, backend s3be.ObjectBackend, loc *core.ObjectLocation) (readpath.ProbeResult[*s3be.GetObjectResult], int64, error) {
	var fail readpath.ProbeResult[*s3be.GetObjectResult]

	if !o.core.Usage().WithinLimits(beName, getObjectOp, 0, 0) {
		return fail, 0, fmt.Errorf("backend %s: %w", beName, readpath.ErrUsageLimitSkip)
	}
	// Encrypted reads need the location row to unwrap the DEK; without it
	// (degraded broadcast with the DB unreachable) we cannot decrypt.
	if o.encryptor != nil && loc == nil {
		return fail, 0, core.ErrServiceUnavailable
	}

	// A compressed copy is read by decoding it, which is driven by the codec
	// rather than by a GET of the whole object.
	if isCompressed(loc) {
		res, err := o.compressedGetAttempt(ctx, key, rangeHeader, beName, backend, loc)
		return res, 0, err
	}

	// Reject a copy whose row contradicts itself before doing range math on
	// its sizes. Failing here fails over to a sibling copy, so one damaged row
	// costs a retry rather than a wrong response body.
	if err := core.ValidateEncryptionMetadata(loc); err != nil {
		telemetry.EncryptionFlagMismatchTotal.WithLabelValues("get").Inc()
		return fail, 0, fmt.Errorf("backend %s: %w", beName, err)
	}

	br, err := o.resolveBackendRange(rangeHeader, loc)
	if err != nil {
		observe.RecordSpanError(trace.SpanFromContext(ctx), err)
		o.log.WarnContext(ctx, "range translation failed",
			"key", key, "backend", beName, "range", rangeHeader, "error", err)
		return fail, 0, err
	}
	actualRange := br.header

	r, cancel, err := o.core.GetWithTimeout(ctx, backend, key, actualRange)
	if err != nil {
		o.core.Acct().APICall(s3op.GetObject, beName)
		return fail, 0, err
	}
	wire := r.Size
	if !o.core.Usage().WithinLimits(beName, getObjectOp, wire, 0) {
		_ = r.Body.Close()
		cancel()
		o.core.Acct().APICall(s3op.GetObject, beName)
		return fail, 0, fmt.Errorf("backend %s egress: %w", beName, readpath.ErrUsageLimitSkip)
	}

	r.LastModified = resolveLastModified(r.LastModified, loc)

	if err := verifyStoredEnvelope(r, loc, actualRange); err != nil {
		telemetry.EncryptionFlagMismatchTotal.WithLabelValues("get").Inc()
		_ = r.Body.Close()
		cancel()
		return fail, 0, fmt.Errorf("backend %s: %w", beName, err)
	}

	if err := o.buildPlaintextReader(ctx, r, loc, key, beName, backend, br); err != nil {
		_ = r.Body.Close()
		cancel()
		return fail, 0, err
	}

	// The body owns the timeout cancel via Close. The winner's body streams to
	// the client (cancel fires when the client closes it); a losing result is
	// released by Cleanup, which closes the body and so cancels the timeout.
	r.Body = ioutilx.WithCancel(r.Body, cancel)
	return readpath.ProbeResult[*s3be.GetObjectResult]{
		Value:   r,
		Size:    r.Size,
		Cleanup: func() { _ = r.Body.Close() },
	}, wire, nil
}

// backendRange is a client Range header translated into what the backend
// should actually be asked for.
// For an encrypted object the header is in ciphertext coordinates, and rng
// carries the chunk math needed to slice the plaintext back out; both are the
// zero value when no translation happened.
type backendRange struct {
	header         string // empty for a whole-object read
	rng            *encryption.RangeResult
	ptStart, ptEnd int64 // the plaintext bounds the client asked for
}

// resolveBackendRange translates a plaintext Range header into the ciphertext
// range to request from the backend. An unencrypted object passes its header
// through verbatim, since its stored bytes are already in the coordinates the
// client used.
//
// A range that cannot be translated is never sent to the backend as-is.
// Plaintext offsets addressed against ciphertext select the wrong bytes
// entirely, so a failure here has to surface rather than fall back.
func (o *Manager) resolveBackendRange(rangeHeader string, loc *core.ObjectLocation) (backendRange, error) {
	if loc == nil || !loc.Encrypted || rangeHeader == "" {
		return backendRange{header: rangeHeader}, nil
	}
	ptStart, ptEnd, ok := ParsePlaintextRange(rangeHeader, loc.PlaintextSize)
	if !ok {
		// Unparseable or unsatisfiable against this object. RFC 9110 lets a
		// server ignore a Range it cannot act on, and reading the whole object
		// is the one safe answer: the alternative was handing the backend
		// plaintext offsets for ciphertext.
		return backendRange{}, nil
	}
	rng, err := encryption.CiphertextRange(ptStart, ptEnd, o.encryptor.ChunkSize())
	if err != nil {
		return backendRange{}, fmt.Errorf("%w: %w", core.ErrInvalidRange, err)
	}
	return backendRange{header: rng.BackendRange, rng: rng, ptStart: ptStart, ptEnd: ptEnd}, nil
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
	br backendRange,
) error {
	if loc != nil && loc.Encrypted && o.encryptor != nil {
		if err := decryptResponse(ctx, o.encryptor, r, loc, br.rng, br.ptStart, br.ptEnd); err != nil {
			return err
		}
	}
	o.maybeWrapIntegrityReader(ctx, r, loc, key, beName, backend, br.header != "")
	return nil
}

// maybeWrapIntegrityReader replaces r.Body with a verifying reader when
// integrity verification is enabled and an expected content hash is
// available. A hash mismatch logs, increments telemetry, and enqueues the
// bad copy for cleanup.
//
// A partial response is never verified. The stored hash covers the whole
// object, so a slice of it cannot match, and the mismatch handler destroys the
// copy: without this guard a healthy object loses one copy per ranged read.
// Verifying a range would need per-chunk digests, which the schema does not
// carry, so range coverage belongs to the scrubber, which reads whole objects.
func (o *Manager) maybeWrapIntegrityReader(
	ctx context.Context,
	r *s3be.GetObjectResult,
	loc *core.ObjectLocation,
	key, beName string,
	backend s3be.ObjectBackend,
	partial bool,
) {
	icfg := o.integrityCfg.Load()
	if icfg == nil || !icfg.Enabled || !icfg.VerifyOnRead || partial {
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
		o.dropCorruptedLocation(ctx, key, beName)
	})
	r.Body = vr
}

// dropCorruptedLocation removes the ledger row for a copy discarded on read.
// Without it the replicator still counts the copy and never rebuilds the
// object, leaving it below its replication factor until a reconcile sweeps
// the stale row.
func (o *Manager) dropCorruptedLocation(ctx context.Context, key, beName string) {
	if err := o.stores.DeleteObjectLocation(ctx, key, beName); err != nil {
		o.log.ErrorContext(ctx, "failed to drop location for corrupted copy",
			"key", key, "backend", beName, "error", err)
		return
	}
	o.cache.Delete(key)
}

// applyStoredIdentity replaces the serving copy's answer with the object's
// own, which is the point of storing it: the backend reports a digest of the
// bytes it holds, so an unranged GET that failed over to a replica would
// otherwise hand the client a different validator for an unchanged object.
//
// loc is nil on a degraded-mode broadcast, where there is no row to prefer.
func applyStoredIdentity(res *s3be.GetObjectResult, loc *core.ObjectLocation) {
	if res == nil || loc == nil || !loc.Identity.Complete() {
		return
	}
	res.ETag = loc.Identity.ETag
	if loc.Identity.ContentType != "" {
		res.ContentType = loc.Identity.ContentType
	}
}

// populateObjectCache tees the response body into a pre-sized buffer as it
// streams to the client, handing the buffer to the cache only on a clean read
// of exactly result.Size bytes. An early disconnect, a mid-stream error or a
// short read leaves the cache untouched.
//
// A disabled cache, a ranged request, a non-positive Content-Length or a size
// over the per-entry admission limit all skip the tee entirely, so the body
// streams through with no proxy-side buffering.
func (o *Manager) populateObjectCache(key, rangeHeader string, result *s3be.GetObjectResult, tagCount int) error {
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
		TagCount:     tagCount,
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

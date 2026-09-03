// -------------------------------------------------------------------------------
// Object Manager - PUT
//
// Author: Alex Freidah
//
// PutObject orchestration: body materialization, write failover across
// eligible backends, per-attempt payload construction (encryption + integrity
// hash), pending-intent recovery, and drain-race close. Successful PUT
// finalization (accounting + observability + cache invalidation) lives in
// mutation_finalize.go.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/etag"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/materialize"

	"go.opentelemetry.io/otel/trace"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// PutObject uploads an object to the first backend with available quota.
// If the upload fails, it retries on remaining eligible backends before
// returning an error to the caller (write failover).
// PutObjectRequest is one PutObject call's inputs. Bundled rather than passed
// positionally because the list had already reached the point where two
// adjacent strings could be transposed without the compiler noticing.
//
// Tags are the set the write carries, which replaces whatever the key held: a
// PUT is a full replacement, so an empty Tags leaves the object untagged even
// if its predecessor had tags.
type PutObjectRequest struct {
	Key         string
	Body        io.Reader
	Size        int64
	ContentType string
	Metadata    map[string]string
	Tags        []core.Tag
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

func (o *Manager) PutObject(ctx context.Context, req *PutObjectRequest) (string, error) {
	const operation = s3op.PutObject
	key, size := req.Key, req.Size
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation.String(),
		telemetry.AttrObjectKey.String(key),
		telemetry.AttrObjectSize.Int64(size),
	)
	defer span.End()

	// An uncompressed write is checked against quota before its body is
	// buffered, so a cluster with no room rejects without spending a tempfile
	// on it. A compressed write cannot be: the bytes that will land are not
	// known until the body is encoded, and rejecting on the logical size would
	// turn away a write that fits.
	compress := o.compressOnWrite(size)
	eligible := []string{}
	if !compress {
		if eligible = o.core.EligibleForWrite(putObjectOp, 0, o.physicalSize(size)); len(eligible) == 0 {
			return "", rejectPutForUsage(span, operation)
		}
	}

	plan, err := o.preparePutBody(span, req.Body, size)
	if err != nil {
		return "", err
	}
	defer plan.cleanup()

	if compress {
		if eligible = o.core.EligibleForWrite(putObjectOp, 0, o.physicalSize(plan.storedSize)); len(eligible) == 0 {
			return "", rejectPutForUsage(span, operation)
		}
	}

	// DEK caching: encryptForPut wraps a fresh DEK on first call and
	// reuses it on retries with a new base nonce, sparing the KeyProvider
	// during failover storms.
	var dekState putEncryptState
	var failedBackends []string
	var lastErr error

	for len(eligible) > 0 {
		res := o.attemptPutOnBackend(ctx, span, operation, req, plan, &dekState, eligible)
		if res.fatalErr != nil {
			return "", res.fatalErr
		}
		if res.putErr == nil {
			o.finalizePutSuccess(ctx, span, operation, key, res.backend, res.uploadSize, start, failedBackends)
			return res.etag, nil
		}
		lastErr = res.putErr
		failedBackends = append(failedBackends, res.backend)
		eligible = withoutBackend(eligible, res.backend)
		o.log.WarnContext(ctx, "PutObject: backend write failed, trying next",
			"key", key, "failed_backend", res.backend, "error", res.putErr,
			"remaining_backends", len(eligible))
	}

	observe.RecordSpanError(span, lastErr)
	return "", lastErr
}

// rejectPutForUsage records a write turned away because no backend had room,
// and returns the error PutObject surfaces for it.
func rejectPutForUsage(span trace.Span, operation s3op.Operation) error {
	telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation.String(), "write").Inc()
	observe.MarkSpanError(span, "usage limits exceeded on all backends")
	return core.ErrInsufficientStorage
}

// putPlan is one PUT's payload and the two sizes that describe it. logicalSize
// is what the client wrote and what the object is known by; storedSize is what
// actually lands on a backend, and drives placement, quota and accounting. They
// differ only when the body was compressed.
//
// cleanup releases the materialized plaintext and, when compression ran, the
// encoded copy alongside it. Always safe to call.
type putPlan struct {
	body        *materialize.Body
	logicalSize int64
	storedSize  int64
	contentHash string
	etagDigest  string
	compressed  bool
	cleanup     func()
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// compressOnWrite reports whether a PUT of this size should be encoded. Objects
// under the configured minimum are stored verbatim: a seek table and frame
// headers cost more than a small object saves.
func (o *Manager) compressOnWrite(size int64) bool {
	return o.codec != nil && o.compression.Enabled && size >= o.compression.MinSize
}

// physicalSize reports how many bytes a body of storedSize will occupy on a
// backend once every stored-form layer has been applied.
//
// Encryption is the only layer that can be answered here, and it always can:
// the envelope is a header plus a tag per chunk, so its size is a function of
// the plaintext size and is known before a byte is written. Compression is
// already folded into storedSize by the time this is asked, because an encoder
// only reports its output size once it has run - which is why a compressed
// write is admitted after encoding rather than before.
func (o *Manager) physicalSize(storedSize int64) int64 {
	if o.encryptor == nil {
		return storedSize
	}
	return o.encryptor.CiphertextSize(storedSize)
}

// preparePutBody materializes the request body and, when compression applies,
// encodes it into a second materialized body. Both are held until the upload
// settles: the encoded bytes have to replay on every failover attempt, and the
// plaintext is what they were encoded from.
func (o *Manager) preparePutBody(span trace.Span, src io.Reader, size int64) (*putPlan, error) {
	mbody, etagDigest, contentHash, err := o.bufferPutBody(span, src, size)
	if err != nil {
		return nil, err
	}
	plan := &putPlan{
		body:        mbody,
		logicalSize: size,
		storedSize:  size,
		contentHash: contentHash,
		etagDigest:  etagDigest,
		cleanup:     mbody.Cleanup,
	}
	if !o.compressOnWrite(size) {
		if o.codec != nil && o.compression.Enabled {
			telemetry.CompressionSkippedTotal.WithLabelValues(telemetry.CompressionSkipMinSize).Inc()
		}
		return plan, nil
	}

	cbody, storedSize, err := o.compressPutBody(mbody, size)
	if err != nil {
		mbody.Cleanup()
		telemetry.CompressionErrorsTotal.WithLabelValues(telemetry.CompressionOpEncode).Inc()
		observe.RecordSpanError(span, err)
		return nil, err
	}
	// Encoding an object that did not shrink buys nothing and costs a decode on
	// every later read of it, so the encoded copy is dropped and the plan keeps
	// describing the plaintext it was made from.
	if !compression.WorthStoring(size, storedSize, o.compression.MinRatio) {
		cbody.Cleanup()
		telemetry.CompressionSkippedTotal.WithLabelValues(telemetry.CompressionSkipMinRatio).Inc()
		return plan, nil
	}
	telemetry.RecordCompressed(size, storedSize)
	plan.body, plan.storedSize, plan.compressed = cbody, storedSize, true
	plan.cleanup = func() {
		cbody.Cleanup()
		mbody.Cleanup()
	}
	return plan, nil
}

// compressPutBody encodes the materialized plaintext into a second materialized
// body and reports how many bytes it holds. The size comes from the codec
// rather than the body, which counts only what its own copy loop wrote.
func (o *Manager) compressPutBody(src *materialize.Body, size int64) (*materialize.Body, int64, error) {
	plain, err := src.Reader()
	if err != nil {
		return nil, 0, err
	}
	// Sized by the logical size, since the encoded form is never larger by
	// enough to matter: this spills to disk no later than the plaintext did.
	dst, err := materialize.NewEmpty(size)
	if err != nil {
		return nil, 0, err
	}
	n, err := o.codec.Compress(dst.Writer(), plain)
	if err != nil {
		dst.Cleanup()
		return nil, 0, fmt.Errorf("compress body: %w", err)
	}
	return dst, n, nil
}

// putAttemptResult conveys the outcome of one backend PUT attempt back to
// the failover loop. A non-nil fatalErr terminates the call. A non-nil
// putErr signals a backend-side failure that should drop the chosen
// backend and retry on the remainder.
// uploadSize is what the attempt actually sent, which is what the backend is
// charged. It is the plan's stored size grown by the encryption envelope when
// one was applied, and is carried back rather than recomputed so accounting
// reports the figure the upload used.
type putAttemptResult struct {
	backend    string
	etag       string
	uploadSize int64
	fatalErr   error
	putErr     error
}

// bufferPutBody materializes the request body into a seekable form
// (memory for small payloads, tempfile above materialize.MemThreshold)
// so failover retries can replay the plaintext without holding the
// full body on the heap. Both digests are computed during that single
// buffering pass via io.MultiWriter so the body is not re-scanned
// afterwards.
//
// The ETag's MD5 is unconditional: it is what the client is told the object
// is, so it cannot be gated on an operator's integrity setting the way the
// verification SHA-256 is.
//
// Returns the materialized body, the ETag digest, the content hash (empty
// when integrity verification is disabled), and a cleanup the caller must
// invoke once the upload settles (safe to defer in every code path).
func (o *Manager) bufferPutBody(span trace.Span, body io.Reader, size int64) (*materialize.Body, string, string, error) {
	var hasher hash.Hash
	icfg := o.integrityCfg.Load()
	if icfg != nil && icfg.Enabled {
		hasher = newSHA256()
	}
	etagHasher := etag.NewHasher()
	mb, err := materialize.New(body, size, hasher, etagHasher)
	if err != nil {
		observe.RecordSpanError(span, err)
		return nil, "", "", fmt.Errorf("buffer request body: %w", err)
	}
	return mb, etag.Hex(etagHasher), sha256Hex(hasher), nil
}

// attemptPutOnBackend performs one backend PUT attempt: select a
// destination, prepare the payload (encrypt/hash), insert a pending
// intent, upload, then promote the intent on success.
func (o *Manager) attemptPutOnBackend(ctx context.Context, span trace.Span, operation s3op.Operation, req *PutObjectRequest, plan *putPlan, dekState *putEncryptState, eligible []string) putAttemptResult {
	key := req.Key
	// Placement decides on the bytes that will occupy the backend, which is
	// neither the size the client announced nor the size the plan holds: a
	// compressed write shrank before this point and an encrypted one grows
	// after it.
	backendName, err := o.coord.SelectBackendForWrite(ctx, o.physicalSize(plan.storedSize), eligible)
	if err != nil {
		return putAttemptResult{fatalErr: o.core.ClassifyWriteError(span, operation.String(), err)}
	}
	span.SetAttributes(telemetry.AttrBackendName.String(backendName))

	be, err := o.core.GetBackend(backendName)
	if err != nil {
		observe.RecordSpanError(span, err)
		return putAttemptResult{backend: backendName, fatalErr: err}
	}

	uploadBody, uploadSize, form, err := o.buildPutPayload(ctx, plan, dekState)
	if err != nil {
		observe.RecordSpanError(span, err)
		return putAttemptResult{backend: backendName, fatalErr: err}
	}

	// Insert the pending intent before the backend PUT so a metadata
	// commit failure after the bytes land has a recovery breadcrumb: the
	// pending reaper promotes the intent on a later tick instead of the
	// old failure path silently deleting the just-written copy.
	identity := putIdentity(plan.etagDigest, req)
	intentID, err := o.coord.InsertPendingIntent(ctx, key, backendName, uploadSize, form, identity)
	if err != nil {
		observe.RecordSpanError(span, err)
		return putAttemptResult{backend: backendName, fatalErr: err}
	}

	bctx, bcancel := o.core.WithTimeout(ctx)
	// The backend's ETag is discarded: it describes the bytes as stored, which
	// are ciphertext or compressed frames whenever either feature is on.
	_, err = be.PutObject(bctx, key, uploadBody, uploadSize, req.ContentType, req.Metadata)
	bcancel()
	if err != nil {
		o.core.Acct().APICall(s3op.PutObject, backendName)
		// Leave the pending row for the reaper. A backend PUT error does
		// not reliably mean the bytes are absent: the response could have
		// been lost mid-flight, so the reaper HEADs the backend on its
		// next tick and either promotes or drops the intent.
		return putAttemptResult{backend: backendName, putErr: err}
	}

	// Drain-race close: a drain that started after EligibleForWrite ran
	// could have flipped this backend to draining while the backend PUT
	// was in flight. If finalizeDrain's DeleteBackendData runs after the
	// commit below, the just-promoted row gets wiped and the physical
	// bytes are orphaned. Re-check before the commit so we land on a
	// different backend instead; the pending intent stays for the
	// reaper, which HEADs the backend, sees no object (we just deleted
	// the bytes), and drops the intent.
	if o.core.IsDraining(backendName) {
		o.log.WarnContext(ctx, "drain started mid-write; aborting commit on draining backend",
			"key", key, "backend", backendName)
		telemetry.DrainRaceAbortedTotal.Inc()
		o.coord.RecoverFromRecordFailure(ctx, be, backendName, key, "drain_race_aborted", uploadSize)
		return putAttemptResult{backend: backendName, putErr: errDrainRaceAborted}
	}

	if err := o.coord.RecordObjectAndPromoteIntent(ctx, span, &core.RecordObjectRequest{
		Key: key, Backend: backendName, Size: uploadSize, Form: form, Identity: identity,
		Tags: req.Tags, IntentID: intentID,
	}); err != nil {
		return putAttemptResult{backend: backendName, fatalErr: err}
	}
	// The ETag the client is told is the one recorded, not the one the backend
	// returned: with compression or encryption on those differ, and a later
	// HEAD answers from the row.
	return putAttemptResult{backend: backendName, etag: identity.ETag, uploadSize: uploadSize}
}

// putIdentity assembles what a later read reports for this object without
// asking a backend: the ETag over the bytes the client sent, plus the content
// type and user metadata the request carried. A nil map is normalised to an
// empty one so a stored identity means "this object has no user metadata"
// rather than "nobody looked".
func putIdentity(digest string, req *PutObjectRequest) *core.ObjectIdentity {
	meta := req.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	return &core.ObjectIdentity{
		ETag:         etag.Single(digest),
		ContentType:  req.ContentType,
		UserMetadata: meta,
	}
}

// errDrainRaceAborted is the sentinel putErr the attemptPutOnBackend
// drain-race close returns so the outer failover loop drops the
// draining backend from the eligible set and retries elsewhere
// instead of treating the abort as a generic backend failure.
var errDrainRaceAborted = errors.New("aborted: drain started mid-write")

// buildPutPayload prepares the upload body and StoredForm for a
// single attempt. The materialized body's Reader() rewinds on every
// call so encryption and unencrypted paths both replay from offset 0
// across failover retries. Encryption layering, when enabled, runs
// through encryptForPut so the wrapped DEK is reused across retries.
// Compression, when it ran, is already baked into the body: it encodes once
// ahead of the failover loop, so an attempt replays encoded bytes and only the
// encryption layer is rebuilt per attempt. That ordering is the convention
// encryption established - compress, then encrypt - and it makes the encoded
// stream the encryptor's plaintext domain.
func (o *Manager) buildPutPayload(
	ctx context.Context,
	plan *putPlan,
	dekState *putEncryptState,
) (io.Reader, int64, *core.StoredForm, error) {
	stored, err := plan.body.Reader()
	if err != nil {
		return nil, 0, nil, err
	}
	if o.encryptor != nil {
		uploadBody, uploadSize, form, err := encryptForPut(ctx, o.encryptor, stored, plan.storedSize, dekState)
		if err != nil {
			return nil, 0, nil, err
		}
		form.ContentHash = plan.contentHash
		o.applyCompressionMeta(form, plan)
		return uploadBody, uploadSize, form, nil
	}
	var form *core.StoredForm
	if plan.contentHash != "" || plan.compressed {
		form = &core.StoredForm{ContentHash: plan.contentHash}
		o.applyCompressionMeta(form, plan)
	}
	return stored, plan.storedSize, form, nil
}

// applyCompressionMeta records how the stored bytes were encoded. LogicalSize
// is the only place the client-visible size survives, since the row's
// SizeBytes counts what landed on the backend; it is left zero for a verbatim
// object, where the two are the same and an empty algorithm already says so.
func (o *Manager) applyCompressionMeta(form *core.StoredForm, plan *putPlan) {
	if !plan.compressed {
		return
	}
	form.CompressionAlgorithm = compression.Algorithm
	form.CompressionLevel = o.compression.Level
	form.CompressionFormatVersion = compression.FormatVersion
	form.LogicalSize = plan.logicalSize
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

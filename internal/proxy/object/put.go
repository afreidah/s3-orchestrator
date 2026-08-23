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
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/materialize"

	"go.opentelemetry.io/otel/trace"
)

// PutObject uploads an object to the first backend with available quota.
// If the upload fails, it retries on remaining eligible backends before
// returning an error to the caller (write failover).
func (o *Manager) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error) {
	const operation = "PutObject"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
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
		if eligible = o.core.EligibleForWrite(1, 0, size); len(eligible) == 0 {
			return "", rejectPutForUsage(span, operation)
		}
	}

	plan, err := o.preparePutBody(span, body, size)
	if err != nil {
		return "", err
	}
	defer plan.cleanup()

	if compress {
		if eligible = o.core.EligibleForWrite(1, 0, plan.storedSize); len(eligible) == 0 {
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
		res := o.attemptPutOnBackend(ctx, span, operation, key, plan, contentType, metadata, &dekState, eligible)
		if res.fatalErr != nil {
			return "", res.fatalErr
		}
		if res.putErr == nil {
			o.finalizePutSuccess(ctx, span, operation, key, res.backend, plan.storedSize, start, failedBackends)
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
func rejectPutForUsage(span trace.Span, operation string) error {
	telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "write").Inc()
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
	compressed  bool
	cleanup     func()
}

// compressOnWrite reports whether a PUT of this size should be encoded. Objects
// under the configured minimum are stored verbatim: a seek table and frame
// headers cost more than a small object saves.
func (o *Manager) compressOnWrite(size int64) bool {
	return o.codec != nil && o.compression.Enabled && size >= o.compression.MinSize
}

// preparePutBody materializes the request body and, when compression applies,
// encodes it into a second materialized body. Both are held until the upload
// settles: the encoded bytes have to replay on every failover attempt, and the
// plaintext is what they were encoded from.
func (o *Manager) preparePutBody(span trace.Span, src io.Reader, size int64) (*putPlan, error) {
	mbody, contentHash, err := o.bufferPutBody(span, src, size)
	if err != nil {
		return nil, err
	}
	plan := &putPlan{
		body:        mbody,
		logicalSize: size,
		storedSize:  size,
		contentHash: contentHash,
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
type putAttemptResult struct {
	backend  string
	etag     string
	fatalErr error
	putErr   error
}

// bufferPutBody materializes the request body into a seekable form
// (memory for small payloads, tempfile above materialize.MemThreshold)
// so failover retries can replay the plaintext without holding the
// full body on the heap. When integrity verification is enabled, the
// SHA-256 is computed during the same single buffering pass via
// io.MultiWriter so the body is not re-scanned after materialization.
//
// Returns the materialized body, the content hash (empty when
// integrity verification is disabled), and a cleanup the caller must
// invoke once the upload settles (safe to defer in every code path).
func (o *Manager) bufferPutBody(span trace.Span, body io.Reader, size int64) (*materialize.Body, string, error) {
	var hasher hash.Hash
	icfg := o.integrityCfg.Load()
	if icfg != nil && icfg.Enabled {
		hasher = newSHA256()
	}
	mb, err := materialize.New(body, size, hasher)
	if err != nil {
		observe.RecordSpanError(span, err)
		return nil, "", fmt.Errorf("buffer request body: %w", err)
	}
	return mb, sha256Hex(hasher), nil
}

// attemptPutOnBackend performs one backend PUT attempt: select a
// destination, prepare the payload (encrypt/hash), insert a pending
// intent, upload, then promote the intent on success.
func (o *Manager) attemptPutOnBackend(ctx context.Context, span trace.Span, operation, key string, plan *putPlan, contentType string, metadata map[string]string, dekState *putEncryptState, eligible []string) putAttemptResult {
	// Placement decides on the bytes that will occupy the backend, which for a
	// compressed write is not the size the client announced.
	backendName, err := o.coord.SelectBackendForWrite(ctx, plan.storedSize, eligible)
	if err != nil {
		return putAttemptResult{fatalErr: o.core.ClassifyWriteError(span, operation, err)}
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
	intentID, err := o.coord.InsertPendingIntent(ctx, key, backendName, uploadSize, form)
	if err != nil {
		observe.RecordSpanError(span, err)
		return putAttemptResult{backend: backendName, fatalErr: err}
	}

	bctx, bcancel := o.core.WithTimeout(ctx)
	etag, err := be.PutObject(bctx, key, uploadBody, uploadSize, contentType, metadata)
	bcancel()
	if err != nil {
		o.core.Acct().APICall(backendName)
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

	if err := o.coord.RecordObjectAndPromoteIntent(ctx, span, key, backendName, uploadSize, form, intentID); err != nil {
		return putAttemptResult{backend: backendName, fatalErr: err}
	}
	return putAttemptResult{backend: backendName, etag: etag}
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

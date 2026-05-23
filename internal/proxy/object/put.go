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

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

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

	eligible := o.core.EligibleForWrite(1, 0, size)
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "write").Inc()
		observe.MarkSpanError(span, "usage limits exceeded on all backends")
		return "", core.ErrInsufficientStorage
	}

	mbody, contentHash, err := o.bufferPutBody(span, body, size)
	if err != nil {
		return "", err
	}
	defer mbody.Cleanup()

	// DEK caching: encryptForPut wraps a fresh DEK on first call and
	// reuses it on retries with a new base nonce, sparing the KeyProvider
	// during failover storms.
	var dekState putEncryptState
	var failedBackends []string
	var lastErr error

	for len(eligible) > 0 {
		res := o.attemptPutOnBackend(ctx, span, operation, key, mbody, size, contentType, metadata, contentHash, &dekState, eligible)
		if res.fatalErr != nil {
			return "", res.fatalErr
		}
		if res.putErr == nil {
			o.finalizePutSuccess(ctx, span, operation, key, res.backend, size, start, failedBackends)
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
// (memory for small payloads, tempfile above materializeMemThreshold)
// so failover retries can replay the plaintext without holding the
// full body on the heap. When integrity verification is enabled, the
// SHA-256 is computed during the same single buffering pass via
// io.MultiWriter so the body is not re-scanned after materialization.
//
// Returns the materialized body, the content hash (empty when
// integrity verification is disabled), and a cleanup the caller must
// invoke once the upload settles (safe to defer in every code path).
func (o *Manager) bufferPutBody(span trace.Span, body io.Reader, size int64) (*materializedBody, string, error) {
	var hasher hash.Hash
	icfg := o.integrityCfg.Load()
	if icfg != nil && icfg.Enabled {
		hasher = newSHA256()
	}
	mb, err := newMaterializedBody(body, size, hasher)
	if err != nil {
		observe.RecordSpanError(span, err)
		return nil, "", fmt.Errorf("buffer request body: %w", err)
	}
	return mb, sha256Hex(hasher), nil
}

// attemptPutOnBackend performs one backend PUT attempt: select a
// destination, prepare the payload (encrypt/hash), insert a pending
// intent, upload, then promote the intent on success.
func (o *Manager) attemptPutOnBackend(ctx context.Context, span trace.Span, operation, key string, body *materializedBody, size int64, contentType string, metadata map[string]string, contentHash string, dekState *putEncryptState, eligible []string) putAttemptResult {
	backendName, err := o.coord.SelectBackendForWrite(ctx, size, eligible)
	if err != nil {
		return putAttemptResult{fatalErr: o.core.ClassifyWriteError(span, operation, err)}
	}
	span.SetAttributes(telemetry.AttrBackendName.String(backendName))

	be, err := o.core.GetBackend(backendName)
	if err != nil {
		observe.RecordSpanError(span, err)
		return putAttemptResult{backend: backendName, fatalErr: err}
	}

	uploadBody, uploadSize, enc, err := o.buildPutPayload(ctx, body, size, contentHash, dekState)
	if err != nil {
		observe.RecordSpanError(span, err)
		return putAttemptResult{backend: backendName, fatalErr: err}
	}

	// Insert the pending intent before the backend PUT so a metadata
	// commit failure after the bytes land has a recovery breadcrumb: the
	// pending reaper promotes the intent on a later tick instead of the
	// old failure path silently deleting the just-written copy.
	intentID, err := o.coord.InsertPendingIntent(ctx, key, backendName, uploadSize, enc)
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

	if err := o.coord.RecordObjectAndPromoteIntent(ctx, span, key, backendName, uploadSize, enc, intentID); err != nil {
		return putAttemptResult{backend: backendName, fatalErr: err}
	}
	return putAttemptResult{backend: backendName, etag: etag}
}

// errDrainRaceAborted is the sentinel putErr the attemptPutOnBackend
// drain-race close returns so the outer failover loop drops the
// draining backend from the eligible set and retries elsewhere
// instead of treating the abort as a generic backend failure.
var errDrainRaceAborted = errors.New("aborted: drain started mid-write")

// buildPutPayload prepares the upload body and EncryptionMeta for a
// single attempt. The materialized body's Reader() rewinds on every
// call so encryption and unencrypted paths both replay from offset 0
// across failover retries. Encryption layering, when enabled, runs
// through encryptForPut so the wrapped DEK is reused across retries.
func (o *Manager) buildPutPayload(
	ctx context.Context,
	body *materializedBody,
	size int64,
	contentHash string,
	dekState *putEncryptState,
) (io.Reader, int64, *core.EncryptionMeta, error) {
	plain, err := body.Reader()
	if err != nil {
		return nil, 0, nil, err
	}
	if o.encryptor != nil {
		uploadBody, uploadSize, enc, err := encryptForPut(ctx, o.encryptor, plain, size, dekState)
		if err != nil {
			return nil, 0, nil, err
		}
		enc.ContentHash = contentHash
		return uploadBody, uploadSize, enc, nil
	}
	var enc *core.EncryptionMeta
	if contentHash != "" {
		enc = &core.EncryptionMeta{ContentHash: contentHash}
	}
	return plain, size, enc, nil
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

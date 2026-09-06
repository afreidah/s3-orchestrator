// -------------------------------------------------------------------------------
// Object Manager - PUT
//
// Author: Alex Freidah
//
// PutObject orchestration: body materialization through compression and
// encryption into the exact bytes an upload sends, write failover across
// eligible backends, pending-intent recovery, and drain-race close. Successful
// PUT finalization (accounting + observability + cache invalidation) lives in
// mutation_finalize.go.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/etag"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
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

	plan, err := o.preparePutBody(ctx, span, req.Body, size)
	if err != nil {
		return "", err
	}
	defer plan.cleanup()

	if compress {
		if eligible = o.core.EligibleForWrite(putObjectOp, 0, plan.uploadSize); len(eligible) == 0 {
			return "", rejectPutForUsage(span, operation)
		}
	}

	if o.copiesPerWrite > 1 {
		return o.putCopiesInParallel(ctx, span, req, plan, eligible, start)
	}

	var failedBackends []string
	var lastErr error

	for len(eligible) > 0 {
		res := o.attemptPutOnBackend(ctx, span, operation, req, plan, eligible)
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

// putPlan is one PUT's payload, the sizes that describe it, and the stored form
// a row records it under. body holds the bytes an upload sends, already encoded
// and encrypted, so every upload of this object replays one payload rather than
// building its own.
//
// logicalSize is what the client wrote and what the object is known by;
// storedSize is the plaintext those bytes encode, which is what the encryption
// envelope is measured against; uploadSize is what lands on a backend, and
// drives placement, quota and accounting. They differ only when the body was
// compressed or encrypted.
//
// form describes the stored bytes to the database. It is nil for an object
// stored verbatim by a deployment with integrity hashing off, where there is
// nothing about them a row needs to carry.
//
// readers counts who still needs the payload. A write placing several copies
// answers the client on the first one and returns while the rest are still
// uploading, so the request is not what the body's lifetime can be tied to.
type putPlan struct {
	body        *materialize.Body
	logicalSize int64
	storedSize  int64
	uploadSize  int64
	contentHash string
	etagDigest  string
	compressed  bool
	form        *core.StoredForm
	readers     atomic.Int32
}

// swapBody installs the body a stage produced and releases the one it consumed.
// Nothing reads an earlier stage's bytes once the next stage has materialized
// its own, and holding them would double what a large write occupies.
//
// Preparation is single-threaded and finishes before any upload starts, so the
// bodies swapped through here are never ones a reader is holding.
func (p *putPlan) swapBody(next *materialize.Body) {
	p.body.Cleanup()
	p.body = next
}

// hold registers a reader of the payload, matched by a later cleanup. Taken
// before the reader starts, so the request cannot release the body between the
// decision to read it and the read.
func (p *putPlan) hold() {
	p.readers.Add(1)
}

// cleanup releases the payload once nobody is left reading it. The request
// itself counts as a reader, so a write with no copies still in flight frees
// the body as it returns. Always safe to call.
func (p *putPlan) cleanup() {
	if p.readers.Add(-1) < 0 {
		p.body.Cleanup()
	}
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

// physicalSize reports how many bytes a body of this size will occupy on a
// backend once the stored form has been applied. It is what a write is admitted
// against before its body is buffered.
//
// Encryption is the only layer answerable that early, and it always is: the
// envelope is a header plus a tag per chunk, so its size is a function of the
// plaintext size and is known before a byte is written. Compression is not,
// because an encoder only reports its output size once it has run - which is
// why a compressed write is admitted after its body is prepared, against the
// upload size the plan ended up with.
func (o *Manager) physicalSize(size int64) int64 {
	if o.encryptor == nil {
		return size
	}
	return o.encryptor.CiphertextSize(size)
}

// preparePutBody materializes the request body into the exact bytes an upload
// sends: buffered, then encoded when compression pays, then encrypted when the
// deployment encrypts. Each stage materializes a body of its own and releases
// the one it consumed, so what the plan carries is one payload and the stored
// form describing it.
func (o *Manager) preparePutBody(ctx context.Context, span trace.Span, src io.Reader, size int64) (*putPlan, error) {
	mbody, etagDigest, contentHash, err := o.bufferPutBody(span, src, size)
	if err != nil {
		return nil, err
	}
	plan := &putPlan{
		body:        mbody,
		logicalSize: size,
		storedSize:  size,
		uploadSize:  size,
		contentHash: contentHash,
		etagDigest:  etagDigest,
	}
	if err := o.compressPutPlan(plan); err != nil {
		plan.cleanup()
		observe.RecordSpanError(span, err)
		return nil, err
	}
	if err := o.encryptPutPlan(ctx, plan); err != nil {
		plan.cleanup()
		observe.RecordSpanError(span, err)
		return nil, err
	}
	o.describeStoredBytes(plan)
	return plan, nil
}

// compressPutPlan encodes the plan's payload when compression applies to a
// write of this size, and leaves it alone otherwise. An object that did not
// shrink keeps its plaintext: encoding it buys nothing and costs a decode on
// every later read of it.
func (o *Manager) compressPutPlan(plan *putPlan) error {
	if !o.compressOnWrite(plan.logicalSize) {
		if o.codec != nil && o.compression.Enabled {
			telemetry.CompressionSkippedTotal.WithLabelValues(telemetry.CompressionSkipMinSize).Inc()
		}
		return nil
	}
	cbody, storedSize, err := o.compressPutBody(plan.body, plan.logicalSize)
	if err != nil {
		telemetry.CompressionErrorsTotal.WithLabelValues(telemetry.CompressionOpEncode).Inc()
		return err
	}
	if !compression.WorthStoring(plan.logicalSize, storedSize, o.compression.MinRatio) {
		cbody.Cleanup()
		telemetry.CompressionSkippedTotal.WithLabelValues(telemetry.CompressionSkipMinRatio).Inc()
		return nil
	}
	telemetry.RecordCompressed(plan.logicalSize, storedSize)
	plan.swapBody(cbody)
	plan.storedSize, plan.uploadSize, plan.compressed = storedSize, storedSize, true
	return nil
}

// encryptPutPlan encrypts the plan's payload once, ahead of the failover loop,
// so every upload the write makes sends one identical ciphertext. Encrypting
// per upload would draw a fresh base nonce each time and leave the copies of a
// key differing byte for byte, which nothing downstream can see: the rows stay
// self-describing and each copy reads and scrubs on its own.
//
// Compression, when it ran, is already baked into the body. That ordering is
// the convention encryption established - compress, then encrypt - and it makes
// the encoded stream the encryptor's plaintext domain.
func (o *Manager) encryptPutPlan(ctx context.Context, plan *putPlan) error {
	if o.encryptor == nil {
		return nil
	}
	stored, err := plan.body.Reader()
	if err != nil {
		return err
	}
	ciphertext, uploadSize, form, err := materializeEncrypted(ctx, o.encryptor, stored, plan.storedSize)
	if err != nil {
		return err
	}
	plan.swapBody(ciphertext)
	plan.uploadSize, plan.form = uploadSize, form
	return nil
}

// describeStoredBytes finishes the stored form the encrypt stage started, with
// the integrity hash over what the client sent and how the bytes were encoded.
// A verbatim object written by a deployment with integrity hashing off has
// nothing to describe and keeps a nil form.
func (o *Manager) describeStoredBytes(plan *putPlan) {
	if plan.form == nil {
		if plan.contentHash == "" && !plan.compressed {
			return
		}
		plan.form = &core.StoredForm{}
	}
	plan.form.ContentHash = plan.contentHash
	o.applyCompressionMeta(plan.form, plan)
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
// uploadSize is what the attempt sent, which is what the backend is charged. It
// is carried back rather than read off the plan again so accounting reports the
// figure the upload used.
type putAttemptResult struct {
	backend    string
	etag       string
	uploadSize int64
	fatalErr   error
	putErr     error
}

// bufferPutBody materializes the request body into a seekable form
// (memory for small payloads, tempfile above materialize.MemThreshold)
// so the stages that follow can read it without holding the full body
// on the heap. Both digests are computed during that single buffering
// pass via io.MultiWriter so the body is not re-scanned afterwards -
// which is also why the later stages can release the plaintext.
//
// The ETag's MD5 is unconditional: it is what the client is told the object
// is, so it cannot be gated on an operator's integrity setting the way the
// verification SHA-256 is.
//
// Returns the materialized body, the ETag digest, and the content hash (empty
// when integrity verification is disabled).
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

// attemptPutOnBackend performs one backend PUT attempt: replay the prepared
// payload, claim a destination with a pending intent, upload, then promote the
// intent on success.
func (o *Manager) attemptPutOnBackend(ctx context.Context, span trace.Span, operation s3op.Operation, req *PutObjectRequest, plan *putPlan, eligible []string) putAttemptResult {
	key := req.Key
	// The payload was built once, ahead of the loop; an attempt replays it. The
	// materialized body rewinds on every Reader() call, so a retry sends the
	// same bytes the last attempt sent rather than rebuilding them.
	uploadBody, err := plan.body.Reader()
	if err != nil {
		observe.RecordSpanError(span, err)
		return putAttemptResult{fatalErr: err}
	}
	uploadSize, form := plan.uploadSize, plan.form

	// Claiming and choosing are one step. The intent row is both the recovery
	// breadcrumb a failed commit is resolved from and the bytes admission
	// counts against the backend, so the insert that writes it is the statement
	// that decides whether the backend has room. Ranking only proposes an order
	// to try.
	identity := putIdentity(plan.etagDigest, req)
	intent := writepath.NewPendingIntent(key, uploadSize, form, identity)
	backendName, err := o.coord.ClaimWriteTarget(ctx, intent, eligible)
	if err != nil {
		return putAttemptResult{fatalErr: o.core.ClassifyWriteError(span, operation.String(), err)}
	}
	intentID := intent.IntentID
	span.SetAttributes(telemetry.AttrBackendName.String(backendName))

	be, err := o.core.GetBackend(backendName)
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
		Key: key, Size: uploadSize, Form: form, Identity: identity, Tags: req.Tags,
		Copies: []core.ObjectCopy{{Backend: backendName, IntentID: intentID}},
	}); err != nil {
		// Classified like the placement failure above: with placement decided in
		// memory, the commit is now where a database outage first shows up, and
		// it owes the client the same 503 the old placement query gave it.
		return putAttemptResult{backend: backendName, fatalErr: o.core.ClassifyWriteError(span, operation.String(), err)}
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

// -------------------------------------------------------------------------------
// Object Manager - PUT
//
// Author: Alex Freidah
//
// PutObject orchestration: body materialization through the layers the bucket
// stores objects with into the exact bytes an upload sends, write failover
// across eligible backends, pending-intent recovery, and drain-race close.
// Successful PUT finalization (accounting + observability + cache
// invalidation) lives in mutation_finalize.go.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/etag"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
	"github.com/afreidah/s3-orchestrator/internal/util/materialize"

	"go.opentelemetry.io/otel/trace"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// PutObjectRequest is one PutObject call's inputs. Tags replace whatever the
// key held, so an empty Tags leaves the object untagged.
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

// PutObject uploads an object to the first backend with available quota.
// If the upload fails, it retries on remaining eligible backends before
// returning an error to the caller (write failover).
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
	// buffered. A compressed write is checked after encoding, because judging
	// it on the logical size would reject a write that fits.
	want := o.storedForm(key, size)
	var eligible []string
	var err error
	if !want.Has(core.StoredCompressed) {
		if eligible, err = o.d.Coord.WriteTargets(span, operation, o.physicalSize(size, want)); err != nil {
			return "", err
		}
	}

	plan, err := o.preparePutBody(ctx, span, req.Body, size, want)
	if err != nil {
		return "", err
	}
	defer plan.cleanup()

	if want.Has(core.StoredCompressed) {
		if eligible, err = o.d.Coord.WriteTargets(span, operation, plan.uploadSize); err != nil {
			return "", err
		}
	}

	// A fan-out that cannot be tracked falls through to the loop below, which
	// places one copy and leaves the rest to the replicator. It never places
	// more than the bucket's factor, which the cleaner would remove again.
	factors := o.d.Buckets.Factors()
	if copies := min(o.d.CopiesPerWrite, factors.For(key)); copies > 1 {
		etag, err := o.putCopiesInParallel(ctx, span, req, plan, eligible, start, copies)
		if !errors.Is(err, errFanoutUnavailable) {
			return etag, err
		}
	}

	var failedBackends []string
	var lastErr error

	for len(eligible) > 0 {
		res := o.attemptPutOnBackend(ctx, span, operation, req, plan, eligible)
		if res.fatalErr != nil {
			return "", res.fatalErr
		}
		if res.putErr == nil {
			o.finalizePutSuccess(ctx, span, operation, key, res.backend, plan.uploadSize, start, failedBackends)
			return res.etag, nil
		}
		lastErr = res.putErr
		failedBackends = append(failedBackends, res.backend)
		eligible = slices.DeleteFunc(eligible, func(name string) bool { return name == res.backend })
		o.log.WarnContext(ctx, "PutObject: backend write failed, trying next",
			"key", key, "failed_backend", res.backend, "error", res.putErr,
			"remaining_backends", len(eligible))
	}

	observe.RecordSpanError(span, lastErr)
	return "", lastErr
}

// putPlan is one PUT's payload, its sizes, and the stored form a row records it
// under. body holds the bytes an upload sends, already encoded and encrypted, so
// every upload of this object replays one payload.
//
// logicalSize is what the client wrote, and uploadSize what lands on a backend
// and drives placement, quota and accounting.
//
// form is nil for an object stored verbatim with no content hash. readers
// counts who still needs the payload: a multi-copy write answers the client on
// the first copy while the rest keep uploading, so the body cannot be tied to
// the request's lifetime.
type putPlan struct {
	body        *materialize.Body
	logicalSize int64
	uploadSize  int64
	contentHash string
	etagDigest  string
	form        *core.StoredForm
	readers     atomic.Int32
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

// storedForm returns the layers a write of size bytes to key is stored with:
// its bucket's, less compression when the object is too small to be worth it.
func (o *Manager) storedForm(key string, size int64) core.StoredFlags {
	bucket, _ := internalkey.Split(key)
	want := o.d.Buckets.Form(bucket)
	if !storedform.ShouldCompress(want, &o.d.Compression, o.d.Codec, size) {
		want &^= core.StoredCompressed
	}
	return want
}

// physicalSize reports how many bytes a body of this size will occupy once
// stored in the layers of want, which is what a write is admitted against
// before buffering. Compressed size is only known after encoding runs, so a
// compressed write is admitted again after preparation.
func (o *Manager) physicalSize(size int64, want core.StoredFlags) int64 {
	if !want.Has(core.StoredEncrypted) || o.d.Encryptor == nil {
		return size
	}
	return o.d.Encryptor.CiphertextSize(size)
}

// preparePutBody materializes the request body into the exact bytes an upload
// sends, stored in the layers of want, so what the plan carries is one payload
// and the stored form describing it.
func (o *Manager) preparePutBody(ctx context.Context, span trace.Span, src io.Reader, size int64, want core.StoredFlags) (*putPlan, error) {
	mbody, etagDigest, contentHash, err := o.bufferPutBody(span, src, size, want)
	if err != nil {
		return nil, err
	}
	plan := &putPlan{
		body:        mbody,
		logicalSize: size,
		uploadSize:  size,
		contentHash: contentHash,
		etagDigest:  etagDigest,
	}
	if err := o.reformPutPlan(ctx, plan, want); err != nil {
		plan.cleanup()
		observe.RecordSpanError(span, err)
		return nil, err
	}
	return plan, nil
}

// reformPutPlan stores the plan's payload in the layers of want once, ahead of
// the failover loop, so every upload the write makes sends the same bytes: a
// second seal would draw a fresh base nonce, and replication assumes each copy
// holds the bytes its row describes. An encoding that did not pay is dropped,
// since it buys nothing and costs a decode on every later read. The plaintext
// is let go as soon as a later stage holds the bytes, so a compressed and
// encrypted write never holds three buffers.
func (o *Manager) reformPutPlan(ctx context.Context, plan *putPlan, want core.StoredFlags) error {
	from := &core.StoredForm{ContentHash: plan.contentHash}
	release := func() { plan.body.Cleanup(); plan.body = nil }
	for want.Any(core.StoredLayers) {
		out, err := o.layers.Reform(ctx, plan.body.Reader(), plan.logicalSize, from, want, release)
		if _, declined := errors.AsType[*storedform.Declined](err); declined {
			want &^= core.StoredCompressed
			continue
		}
		if err != nil {
			return err
		}
		plan.body, plan.uploadSize, plan.form = out.Body, out.Size, out.Form
		return nil
	}
	plan.form = storedform.WithContentHash(nil, plan.contentHash)
	return nil
}

// putAttemptResult is the outcome of one backend PUT attempt. A non-nil
// fatalErr ends the call; a non-nil putErr drops the backend and retries on the
// rest.
type putAttemptResult struct {
	backend  string
	etag     string
	fatalErr error
	putErr   error
}

// bufferPutBody materializes the request body (memory below
// materialize.MemThreshold, a tempfile above) and computes both digests in the
// same pass, so later stages can release the plaintext. The ETag MD5 is always
// computed because the client is told it; the content hash is empty when the
// layers of want record none.
func (o *Manager) bufferPutBody(span trace.Span, body io.Reader, size int64, want core.StoredFlags) (*materialize.Body, string, string, error) {
	hasher := storedform.ContentHasher(want)
	etagHasher := etag.NewHasher()
	mb, err := materialize.New(body, size, hasher, etagHasher)
	if err != nil {
		observe.RecordSpanError(span, err)
		return nil, "", "", fmt.Errorf("buffer request body: %w", err)
	}
	return mb, etag.Hex(etagHasher), etag.Hex(hasher), nil
}

// attemptPutOnBackend performs one backend PUT attempt: claim a destination
// with a pending intent, upload the prepared payload, then promote the intent
// on success. The payload was built once, ahead of the failover loop, and every
// attempt replays the same bytes.
func (o *Manager) attemptPutOnBackend(ctx context.Context, span trace.Span, operation s3op.Operation, req *PutObjectRequest, plan *putPlan, eligible []string) putAttemptResult {
	// The intent is both the record a failed commit is resolved from and the
	// bytes admission counts against the backend, so the insert that writes it
	// decides whether the backend has room. Ranking only proposes an order.
	identity := putIdentity(plan.etagDigest, req)
	intent := writepath.NewPendingIntent(req.Key, plan.uploadSize, plan.form, identity, req.Tags)
	if err := o.d.Coord.ClaimWriteTarget(ctx, span, operation, intent, eligible); err != nil {
		return putAttemptResult{fatalErr: err}
	}
	backendName := intent.BackendName
	span.SetAttributes(telemetry.AttrBackendName.String(backendName))

	if err := o.uploadCopy(ctx, req, plan, intent); err != nil {
		return putAttemptResult{backend: backendName, putErr: err}
	}

	if err := o.d.Coord.RecordObjectAndPromoteIntent(ctx, span, operation, intent.RecordFor(plan.form)); err != nil {
		return putAttemptResult{backend: backendName, fatalErr: err}
	}
	// The ETag the client is told is the one recorded, not the one the backend
	// returned: with compression or encryption on those differ, and a later
	// HEAD answers from the row.
	return putAttemptResult{backend: backendName, etag: identity.ETag}
}

// putIdentity assembles what a later read reports for this object without
// asking a backend: the ETag over the bytes the client sent, plus the content
// type and user metadata the request carried. A nil map is normalised to an
// empty one so a stored identity means "this object has no user metadata"
// rather than "nobody looked".
func putIdentity(digest string, req *PutObjectRequest) *core.ObjectIdentity {
	return core.NewObjectIdentity(etag.Single(digest), req.ContentType, req.Metadata)
}

// reasonWriteTargetRefused is the cleanup-queue reason for bytes a write left
// on a backend that stopped accepting writes before the commit.
const reasonWriteTargetRefused = "write_target_refused"

// refuseWriteTarget abandons an upload that landed on a backend which stopped
// accepting writes before the commit: it drops the intent and settles the
// cleanup of its bytes.
func (o *Manager) refuseWriteTarget(ctx context.Context, p *core.PendingObject) {
	telemetry.WriteTargetRefusedTotal.Inc()
	o.d.Coord.Discard(ctx, p, reasonWriteTargetRefused)
}

// errWriteTargetRefused is the sentinel putErr the post-PUT re-check returns
// so the outer failover loop drops the backend from the eligible set and
// retries elsewhere instead of treating the abort as a generic backend failure.
var errWriteTargetRefused = errors.New("aborted: backend stopped accepting writes mid-write")

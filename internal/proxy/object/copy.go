// -------------------------------------------------------------------------------
// Object Manager - COPY
//
// Author: Alex Freidah
//
// CopyObject orchestration: source HEAD across replicas, destination
// selection, the same-backend server-side fast path (tryNativeCopy +
// probeDestAfterAmbiguousCopy), and the materialized stream-through
// fallback. Successful COPY finalization (native + materialized) lives in
// mutation_finalize.go; the per-source buffering helpers used by the
// materialized path live in materialize.go.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"fmt"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// headSourceForCopy walks the source's locations until one HEAD succeeds,
// skipping over-limit and unknown backends, and returns its metadata plus the
// stored form from that location's row. ok=false means no copy was reachable.
// Both copy paths move the stored bytes verbatim, so the destination row must
// carry the source row's stored form unchanged.
func (o *Manager) headSourceForCopy(
	ctx context.Context,
	sourceKey string,
	locations []core.ObjectLocation,
) (int64, string, map[string]string, *core.StoredForm, bool) {
	for i := range locations {
		if !o.core.Usage().WithinLimits(locations[i].BackendName, copyObjectOp, 0, 0) {
			continue
		}
		be, ok := o.core.Backends()[locations[i].BackendName]
		if !ok {
			continue
		}
		headResult, err := o.core.HeadWithTimeout(ctx, be, core.StoragePath(sourceKey, locations[i].StorageKey))
		if err != nil {
			continue
		}
		return headResult.Size, headResult.ContentType, headResult.Metadata,
			core.StoredFormFromLocation(&locations[i]), true
	}
	return 0, "", nil, nil, false
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// CopyObject copies an object from sourceKey to destKey, across backends if
// needed and with read failover across the source's copies. It uses a
// server-side copy when the source has a copy on the destination backend, and
// otherwise materializes the source into a seekable buffer before the PUT: a
// non-seekable body makes the SDK drop Content-Length, which some S3
// implementations (notably OCI) reject with HTTP 411.
func (o *Manager) CopyObject(ctx context.Context, req *CopyObjectRequest) (string, error) {
	const operation = s3op.CopyObject
	sourceKey, destKey := req.SourceKey, req.DestKey
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation.String(),
		attribute.String("s3o.source_key", sourceKey),
		attribute.String("s3o.dest_key", destKey),
	)
	defer span.End()

	tags, err := o.resolveCopyTags(ctx, req)
	if err != nil {
		return "", err
	}

	locations, err := core.ClientLocations(o.stores.GetAllObjectLocations(ctx, sourceKey))
	if err != nil {
		if errors.Is(err, core.ErrObjectNotFound) {
			observe.MarkSpanError(span, "source object not found")
			return "", err
		}
		return "", o.core.ClassifyWriteError(span, operation.String(), err)
	}

	size, contentType, metadata, srcForm, ok := o.headSourceForCopy(ctx, sourceKey, locations)
	if !ok {
		err := fmt.Errorf("failed to head source object from any copy")
		observe.RecordSpanError(span, err)
		return "", err
	}
	span.SetAttributes(telemetry.AttrObjectSize.Int64(size))

	// A copy holds the same bytes the source does, so it carries the same
	// identity: the ETag names the object's content, and re-deriving one from
	// the destination backend's answer would give two identical objects two
	// different validators.
	srcIdentity := copyIdentity(locations, contentType, metadata)

	// The copy claims its destination the way a PUT does, so the intent holds
	// the bytes against the backend and gives a failed commit something the
	// reaper can resolve. A native attempt that falls back claims nothing new.
	intent := writepath.NewPendingIntent(destKey, size, srcForm, srcIdentity)
	destBackendName, err := o.coord.SelectWriteTarget(ctx, span, operation, intent)
	if err != nil {
		return "", err
	}
	intentID := intent.IntentID
	destBackend, err := o.core.GetBackend(destBackendName)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}

	// Same-backend fast path: when the source has a copy on the destination
	// backend, try a server-side copy. Once the bytes have landed its result
	// is final, even a metadata failure, because falling back would copy them
	// a second time.
	if sameBackendCopyEligible(locations, destBackendName) {
		req := &nativeCopyContext{
			span:            span,
			destBackend:     destBackend,
			sourceKey:       sourceKey,
			srcStorageKey:   sameBackendStoragePath(locations, destBackendName, sourceKey),
			destKey:         destKey,
			destStorageKey:  intent.StorageKey,
			destBackendName: destBackendName,
			size:            size,
			contentType:     contentType,
			metadata:        metadata,
			srcForm:         srcForm,
			identity:        srcIdentity,
			tags:            tags,
			intentID:        intentID,
			start:           start,
		}
		if etag, handled, nerr := o.tryNativeCopy(ctx, req); handled {
			return etag, nerr
		}
	}

	src, err := o.materializeCopySource(ctx, sourceKey, size, locations)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}
	defer src.cleanup()

	wctx, wcancel := o.core.WithTimeout(ctx)
	defer wcancel()
	etag, err := destBackend.PutObject(wctx, intent.StorageKey, src.body, size, contentType, metadata)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", fmt.Errorf("failed to write destination: %w", err)
	}

	return o.finalizeMaterializedCopy(ctx, &materializedCopyContext{
		span:            span,
		destBackend:     destBackend,
		sourceKey:       sourceKey,
		destKey:         destKey,
		destStorageKey:  intent.StorageKey,
		srcBackendName:  src.sourceBackend,
		destBackendName: destBackendName,
		size:            size,
		srcForm:         srcForm,
		identity:        srcIdentity,
		tags:            tags,
		intentID:        intentID,
		start:           start,
	}, etag)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// copyIdentity gives the destination the source's identity when the source has
// one. The content type and metadata are the ones the copy is written with, so
// they hold whether or not the source row carried an ETag; without that ETag
// there is nothing worth recording, and the destination learns its own on the
// first read that has to ask.
func copyIdentity(locations []core.ObjectLocation, contentType string, metadata map[string]string) *core.ObjectIdentity {
	if len(locations) == 0 || !locations[0].Identity.Complete() {
		return nil
	}
	if metadata == nil {
		metadata = map[string]string{}
	}
	return &core.ObjectIdentity{
		ETag:         locations[0].Identity.ETag,
		ContentType:  contentType,
		UserMetadata: metadata,
	}
}

// sameBackendStoragePath returns the storage path of the source's copy on
// destBackendName, which is what a server-side copy reads. It falls back to the
// bare source key when there is no such copy, which callers rule out first
// with sameBackendCopyEligible.
func sameBackendStoragePath(locations []core.ObjectLocation, destBackendName, sourceKey string) string {
	for i := range locations {
		if locations[i].BackendName == destBackendName {
			return core.StoragePath(sourceKey, locations[i].StorageKey)
		}
	}
	return sourceKey
}

// sameBackendCopyEligible reports whether the source has at least one
// replica on destBackendName. The orchestrator only triggers the
// server-side copy fast path when both keys live on the same backend
// because no backend can satisfy a CopySource that points at a
// foreign bucket.
func sameBackendCopyEligible(locations []core.ObjectLocation, destBackendName string) bool {
	for i := range locations {
		if locations[i].BackendName == destBackendName {
			return true
		}
	}
	return false
}

// CopyObjectRequest is one CopyObject call's inputs.
//
// ReplaceTags carries the x-amz-tagging-directive: false is COPY, which gives
// the destination the source's tag set, and true is REPLACE, which gives it
// Tags instead. A REPLACE with no Tags leaves the destination untagged, which
// is how a client strips a copy's tags.
type CopyObjectRequest struct {
	SourceKey   string
	DestKey     string
	ReplaceTags bool
	Tags        []core.Tag
}

// resolveCopyTags settles which tag set the destination gets.
//
// Read before the copy starts rather than at the finalizers, so both the
// native and the stream-through path commit the same set and neither has to
// reach back to the source once the bytes have moved.
func (o *Manager) resolveCopyTags(ctx context.Context, req *CopyObjectRequest) ([]core.Tag, error) {
	if req.ReplaceTags {
		return req.Tags, nil
	}
	tags, err := o.stores.GetObjectTags(ctx, req.SourceKey)
	if err != nil {
		return nil, fmt.Errorf("read source tags: %w", err)
	}
	return tags, nil
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// materializedCopyContext is what the stream-through copy's finalizer needs.
type materializedCopyContext struct {
	destStorageKey string

	span            trace.Span
	destBackend     s3be.ObjectBackend
	sourceKey       string
	destKey         string
	srcBackendName  string
	destBackendName string
	size            int64
	srcForm         *core.StoredForm
	identity        *core.ObjectIdentity
	tags            []core.Tag
	intentID        string
	start           time.Time
}

// nativeCopyContext is the per-operation state shared by tryNativeCopy,
// probeDestAfterAmbiguousCopy and finalizeNativeCopy.
type nativeCopyContext struct {
	srcStorageKey  string
	destStorageKey string

	span            trace.Span
	destBackend     s3be.ObjectBackend
	sourceKey       string
	destKey         string
	destBackendName string
	size            int64
	contentType     string
	metadata        map[string]string
	srcForm         *core.StoredForm
	identity        *core.ObjectIdentity
	tags            []core.Tag
	intentID        string
	start           time.Time
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// tryNativeCopy attempts a server-side CopyObject on req.destBackend and, on
// success, records the destination and its accounting. The second return is
// whether the bytes reached the destination; when it is true the caller must
// not fall back, which would copy them a second time.
//
// A non-capability error HEAD-probes the destination first, because a backend
// can complete the copy and still lose the response. A native copy is charged
// as one API call with no egress or ingress.
func (o *Manager) tryNativeCopy(ctx context.Context, req *nativeCopyContext) (string, bool, error) {
	copier, ok := req.destBackend.(s3be.Copier)
	if !ok {
		return "", false, nil
	}
	cctx, ccancel := o.core.WithTimeout(ctx)
	defer ccancel()
	etag, err := copier.CopyObject(cctx, req.srcStorageKey, req.destStorageKey, req.contentType, req.metadata)
	if err == nil {
		return o.finalizeNativeCopy(ctx, req, etag)
	}
	if errors.Is(err, s3be.ErrCopyNotSupported) {
		return "", false, nil
	}
	// Ambiguous failure: probe the destination via HEAD. If the
	// destination exists with the expected size, the copy completed
	// server-side; treat it as success and run the same post-success
	// path. Otherwise fall back to materialized copy.
	if recoveredETag, ok := o.probeDestAfterAmbiguousCopy(ctx, req, err); ok {
		return o.finalizeNativeCopy(ctx, req, recoveredETag)
	}
	o.log.WarnContext(ctx, "native copy failed, falling back to materialized copy",
		"source_key", req.sourceKey, "dest_key", req.destKey, "backend", req.destBackendName, "error", err)
	return "", false, nil
}

// probeDestAfterAmbiguousCopy HEADs the destination after a native-copy error
// to tell a lost response from a failed copy. It returns (etag, true) when the
// destination exists with the expected size, and ("", false) otherwise so the
// caller falls back. HEAD errors other than 404 are logged as warnings.
func (o *Manager) probeDestAfterAmbiguousCopy(ctx context.Context, req *nativeCopyContext, origErr error) (string, bool) {
	head, headErr := o.core.HeadWithTimeout(ctx, req.destBackend, req.destStorageKey)
	base := []any{
		"source_key", req.sourceKey,
		"dest_key", req.destKey,
		"backend", req.destBackendName,
		"copy_error", origErr,
	}
	switch {
	case headErr != nil:
		if !s3be.IsNotFound(headErr) {
			o.log.WarnContext(ctx, "ambiguous native-copy HEAD probe failed",
				append(base, "probe_error", headErr)...)
		}
		return "", false
	case head.Size != req.size:
		o.log.WarnContext(ctx, "ambiguous native-copy destination size mismatch, falling back to materialized copy",
			append(base, "expected_size", req.size, "observed_size", head.Size)...)
		return "", false
	default:
		o.log.InfoContext(ctx, "ambiguous native-copy resolved via HEAD probe, destination already populated",
			append(base, "size", head.Size)...)
		return head.ETag, true
	}
}

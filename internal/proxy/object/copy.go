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
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// headSourceForCopy walks the source's known locations until one HEAD
// succeeds (skipping over-limit and unknown backends), and returns its
// metadata plus optional encryption descriptor. ok=false signals that no
// copy could be reached.
func (o *Manager) headSourceForCopy(
	ctx context.Context,
	sourceKey string,
	locations []core.ObjectLocation,
) (int64, string, map[string]string, *core.EncryptionMeta, bool) {
	for i := range locations {
		if !o.core.Usage().WithinLimits(locations[i].BackendName, 1, 0, 0) {
			continue
		}
		be, ok := o.core.Backends()[locations[i].BackendName]
		if !ok {
			continue
		}
		headResult, err := o.core.HeadWithTimeout(ctx, be, sourceKey)
		if err != nil {
			continue
		}
		var srcEnc *core.EncryptionMeta
		if locations[i].Encrypted {
			srcEnc = &core.EncryptionMeta{
				Encrypted:     true,
				EncryptionKey: locations[i].EncryptionKey,
				KeyID:         locations[i].KeyID,
				PlaintextSize: locations[i].PlaintextSize,
				ContentHash:   locations[i].ContentHash,
			}
		}
		return headResult.Size, headResult.ContentType, headResult.Metadata, srcEnc, true
	}
	return 0, "", nil, nil, false
}

// CopyObject copies an object from sourceKey to destKey. Materializes
// the source body into a seekable buffer  -  in-memory for small
// objects, a self-unlinking tempfile above materialize.MemThreshold
// -  before handing it to the destination PutObject. A non-seekable
// body would force the AWS SDK onto its streaming-unsigned-payload
// signing path, which uses chunked transfer encoding and drops
// Content-Length; S3 implementations that require Content-Length
// (notably OCI) then reject the upload with HTTP 411. Supports
// cross-backend copies and read failover from replicas.
func (o *Manager) CopyObject(ctx context.Context, sourceKey, destKey string) (string, error) {
	const operation = "CopyObject"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
		attribute.String("s3o.source_key", sourceKey),
		attribute.String("s3o.dest_key", destKey),
	)
	defer span.End()

	locations, err := o.stores.GetAllObjectLocations(ctx, sourceKey)
	if err != nil {
		if errors.Is(err, core.ErrObjectNotFound) {
			observe.MarkSpanError(span, "source object not found")
			return "", err
		}
		return "", o.core.ClassifyWriteError(span, operation, err)
	}

	size, contentType, metadata, srcEnc, ok := o.headSourceForCopy(ctx, sourceKey, locations)
	if !ok {
		err := fmt.Errorf("failed to head source object from any copy")
		observe.RecordSpanError(span, err)
		return "", err
	}
	span.SetAttributes(telemetry.AttrObjectSize.Int64(size))

	destBackendName, err := o.coord.SelectWriteTarget(ctx, span, operation, size)
	if err != nil {
		return "", err
	}
	destBackend, err := o.core.GetBackend(destBackendName)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", err
	}

	// Same-backend fast path: when a source replica lives on the chosen
	// destination backend and that backend supports server-side copy,
	// skip the materialize+PUT round trip. Falls back to the slow path
	// on ErrCopyNotSupported or any native-call backend error. A
	// post-copy failure (e.g., metadata record failure) surfaces to the
	// caller because the bytes are already on the destination; falling
	// back would copy them a second time.
	if sameBackendCopyEligible(locations, destBackendName) {
		req := &nativeCopyContext{
			span:            span,
			destBackend:     destBackend,
			sourceKey:       sourceKey,
			destKey:         destKey,
			destBackendName: destBackendName,
			size:            size,
			contentType:     contentType,
			metadata:        metadata,
			srcEnc:          srcEnc,
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

	// Seekable body keeps the SDK on UNSIGNED-PAYLOAD signing so
	// Content-Length survives to the backend. The dest PUT goes
	// through the centralized backend-timeout policy so a stalled
	// destination cannot tie up the request past the configured
	// backend_timeout.
	wctx, wcancel := o.core.WithTimeout(ctx)
	defer wcancel()
	etag, err := destBackend.PutObject(wctx, destKey, src.body, size, contentType, metadata)
	if err != nil {
		observe.RecordSpanError(span, err)
		return "", fmt.Errorf("failed to write destination: %w", err)
	}

	return o.finalizeMaterializedCopy(ctx, span, destBackend, sourceKey, destKey, src.sourceBackend, destBackendName, size, srcEnc, start, etag)
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

// nativeCopyContext is the per-operation state the three native-copy
// helpers share: tryNativeCopy attempts the server-side copy,
// probeDestAfterAmbiguousCopy disambiguates lost-response failures,
// and finalizeNativeCopy commits the destination metadata.
type nativeCopyContext struct {
	span            trace.Span
	destBackend     s3be.ObjectBackend
	sourceKey       string
	destKey         string
	destBackendName string
	size            int64
	contentType     string
	metadata        map[string]string
	srcEnc          *core.EncryptionMeta
	start           time.Time
}

// tryNativeCopy attempts a server-side CopyObject on req.destBackend
// and, on success, records the destination location, updates
// accounting, and emits the completion observability. Returns:
//
//   - (etag, true, nil):  native copy + record both succeeded (the
//                         success may have been confirmed via the
//                         HEAD-probe recovery path; either way the
//                         caller treats this as a successful copy)
//   - (_, true, err):     native copy succeeded but a post-step failed;
//                         bytes are already on the destination, so the
//                         caller MUST NOT fall back (that would copy
//                         the bytes a second time)
//   - (_, false, nil):    backend does not support native copy, or the
//                         native call failed AND a HEAD probe confirmed
//                         the destination is not in the expected state;
//                         caller falls back to the materialized copy path
//
// Native copy accounting differs from the materialized path: one API
// call against the destination backend with no egress and no ingress,
// because the bytes never traverse the orchestrator's network.
//
// On a non-capability native-copy error, the destination is HEAD-probed
// before the function decides whether to surface the error or fall back.
// This guards against the ambiguous case where the backend completed the
// copy server-side but the response was lost (timeout, dropped connection)
// - falling back blindly would duplicate the work.
func (o *Manager) tryNativeCopy(ctx context.Context, req *nativeCopyContext) (string, bool, error) {
	copier, ok := req.destBackend.(s3be.BackendCopier)
	if !ok {
		return "", false, nil
	}
	cctx, ccancel := o.core.WithTimeout(ctx)
	defer ccancel()
	etag, err := copier.CopyObject(cctx, req.sourceKey, req.destKey, req.contentType, req.metadata)
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

// probeDestAfterAmbiguousCopy HEADs the destination after a non-
// capability native-copy error to disambiguate "copy succeeded server-
// side but the response was lost" from "copy actually failed." Returns
// (etag, true) when the destination exists and the size matches the
// expected source size; ("", false) otherwise so the caller falls
// back to materialized copy. A 404 on the HEAD is a clean fallback
// signal; any other HEAD error is also a fallback but is logged as a
// warn so operators see the probe failure mode.
func (o *Manager) probeDestAfterAmbiguousCopy(ctx context.Context, req *nativeCopyContext, origErr error) (string, bool) {
	head, headErr := o.core.HeadWithTimeout(ctx, req.destBackend, req.destKey)
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

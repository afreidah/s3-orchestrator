// -------------------------------------------------------------------------------
// Admin API - Compression Handlers
//
// Author: Alex Freidah
//
// The two bulk compression endpoints. Enabling compression only affects objects
// written afterwards, so these are how an operator brings a fleet that already
// holds data under the feature, and how they take it back out.
//
// Both are synchronous and walk the whole ledger, which is why the web UI drives
// them through its own background-job wrapper rather than calling them from a
// request the browser waits on.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"fmt"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// bulkCompressionPass is either direction of the rewrite, which differ only in
// which listing they walk. Naming the shape lets one streaming helper serve
// both rather than each direction carrying its own copy of the plumbing.
type bulkCompressionPass func(context.Context, progress.Observer, int) (ops.BulkRewriteResult, error)

// streamCompression runs one pass as an NDJSON step stream, reporting each
// object as it is rewritten.
//
// These passes read and rewrite every object in a fleet, so they are the
// longest-running thing the admin API offers. A caller watching one needs to
// see it move: a single JSON summary at the end is indistinguishable from a
// hung request until it arrives.
//
// The summary names skipped objects separately, because a pass over media
// declines almost everything and a count that folded those into failures would
// read as a broken run.
// The pass runs under the request context, so a caller that disconnects stops
// the work rather than leaving a fleet-wide rewrite running unwatched. That
// matches every other streaming pass; the web UI wraps these in its own
// background job when it wants them to outlive the request.
func (h *Handler) streamCompression(w http.ResponseWriter, r *http.Request, op, verb string, run bulkCompressionPass, maxObjects int) {
	h.streamSteps(w, op, verb, true, func(obs progress.Observer) (stepResult, error) {
		res, err := run(r.Context(), obs, maxObjects)
		if reason, skipped := skipReason(err); skipped {
			return stepResult{Skipped: reason}, nil
		}
		if err != nil {
			return stepResult{}, err
		}
		return stepResult{
			Processed: res.Succeeded,
			Summary: fmt.Sprintf("rewrote %d, skipped %d, failed %d, of %d",
				res.Succeeded, res.Skipped, res.Failed, res.Total),
			Fields: map[string]any{
				"rewritten": res.Succeeded,
				"skipped":   res.Skipped,
				"failed":    res.Failed,
				"total":     res.Total,
			},
		}, nil
	})
}

// handleCompressExisting encodes every copy currently stored verbatim. Streams
// per-object NDJSON progress when the client accepts the stream content type;
// otherwise returns a single JSON result.
//
// The optional max query parameter caps how many copies this request rewrites,
// 0 or absent meaning the whole fleet. A capped request needs nothing carried
// back for the next one: the copies it converts leave the listing, and the ones
// it declines on ratio are recorded so they leave it too.
func (h *Handler) handleCompressExisting(w http.ResponseWriter, r *http.Request) {
	maxObjects := httputil.QueryPositiveInt(r.URL.Query().Get(paramMax))

	if acceptsStream(r) {
		h.streamCompression(w, r, "compress-existing", "compressing", h.compression.CompressExisting, maxObjects)
		return
	}

	res, err := h.compression.CompressExisting(r.Context(), nil, maxObjects)
	if !h.writeBulkRewriteError(w, r, err, "failed to list uncompressed objects") {
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.CompressExistingResponse{
		BulkCompressionOutcome: adminapi.BulkCompressionOutcome{
			Status:  statusComplete,
			Skipped: res.Skipped,
			Failed:  res.Failed,
			Total:   res.Total,
		},
		Compressed: res.Succeeded,
	})
}

// handleDecompressExisting rewrites every encoded copy back to the bytes the
// client wrote. Streams the same way as the forward pass, and takes the same
// optional max.
func (h *Handler) handleDecompressExisting(w http.ResponseWriter, r *http.Request) {
	maxObjects := httputil.QueryPositiveInt(r.URL.Query().Get(paramMax))

	if acceptsStream(r) {
		h.streamCompression(w, r, "decompress-existing", "decompressing", h.compression.DecompressExisting, maxObjects)
		return
	}

	res, err := h.compression.DecompressExisting(r.Context(), nil, maxObjects)
	if !h.writeBulkRewriteError(w, r, err, "failed to list compressed objects") {
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.DecompressExistingResponse{
		BulkCompressionOutcome: adminapi.BulkCompressionOutcome{
			Status:  statusComplete,
			Skipped: res.Skipped,
			Failed:  res.Failed,
			Total:   res.Total,
		},
		Decompressed: res.Succeeded,
	})
}

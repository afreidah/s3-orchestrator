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
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// handleCompressExisting encodes every copy currently stored verbatim.
func (h *Handler) handleCompressExisting(w http.ResponseWriter, r *http.Request) {
	res, err := h.compression.CompressExisting(r.Context())
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
// client wrote.
func (h *Handler) handleDecompressExisting(w http.ResponseWriter, r *http.Request) {
	res, err := h.compression.DecompressExisting(r.Context())
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

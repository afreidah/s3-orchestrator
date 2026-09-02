// -------------------------------------------------------------------------------
// Admin API - On-Demand Lifecycle Expiration
//
// Author: Alex Freidah
//
// POST /admin/api/lifecycle runs one expiration sweep so an operator who has
// just written or corrected a rule can find out whether it matches anything.
// Without it the only way to make a sweep happen is to wait out the hourly
// tick plus its startup jitter, and until that passes a rule matching nothing
// is indistinguishable from a rule that ran and found nothing expired.
//
// A sweep over a large expired backlog runs for minutes, so it streams a line
// per object when the caller asks; the sweep itself lives in internal/ops.
// -------------------------------------------------------------------------------

package admin

import (
	"fmt"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// handleLifecycle applies every configured lifecycle rule once and reports what
// it removed. Streams per-object NDJSON progress when the client accepts the
// stream content type; otherwise returns a single JSON result.
func (h *Handler) handleLifecycle(w http.ResponseWriter, r *http.Request) {
	if acceptsStream(r) {
		h.streamLifecycle(w, r)
		return
	}

	res, err := h.expiry.Run(r.Context(), nil)
	if reason, skipped := skipReason(err); skipped {
		httputil.WriteJSON(w, http.StatusOK, adminapi.LifecycleResponse{Status: statusSkipped, Reason: reason})
		return
	}
	if err != nil {
		h.internalError(r.Context(), w, "lifecycle sweep failed", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.LifecycleResponse{
		Status:  statusOK,
		Deleted: res.Deleted,
		Failed:  res.Failed,
	})
}

// streamLifecycle runs a sweep as an NDJSON step stream, one "expiring <key>"
// line per object plus a terminal summary. Objects are deleted one at a time,
// so steps render as a live prefix completed by their status (sequential=true).
func (h *Handler) streamLifecycle(w http.ResponseWriter, r *http.Request) {
	h.streamSteps(w, "lifecycle", "expiring", true, func(obs progress.Observer) (stepResult, error) {
		res, err := h.expiry.Run(r.Context(), obs)
		if reason, skipped := skipReason(err); skipped {
			return stepResult{Skipped: reason}, nil
		}
		if err != nil {
			return stepResult{}, err
		}
		return stepResult{
			Processed: res.Deleted,
			Summary:   fmt.Sprintf("expired %d objects", res.Deleted),
			Fields:    map[string]any{"deleted": res.Deleted, "failed": res.Failed},
		}, nil
	})
}

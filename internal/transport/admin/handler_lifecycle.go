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
// The sweep itself lives in internal/ops.
// -------------------------------------------------------------------------------

package admin

import (
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// handleLifecycle applies every configured lifecycle rule once and reports what
// it removed.
//
// No streaming variant: ProcessRules takes no observer, and a plain JSON answer
// is enough for the question the endpoint exists to answer.
func (h *Handler) handleLifecycle(w http.ResponseWriter, r *http.Request) {
	res, err := h.expiry.Run(r.Context())
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

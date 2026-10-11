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

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// handleLifecycle applies every configured lifecycle rule once and reports what
// it removed. Objects are deleted one at a time, so streamed steps render as a
// live prefix completed by their status.
func (h *Handler) handleLifecycle(w http.ResponseWriter, r *http.Request) {
	h.servePass(w, r, passEndpoint[batch.Summary]{
		op:         "lifecycle",
		verb:       "expiring",
		sequential: true,
		failMsg:    "lifecycle sweep failed",
		run:        h.expiry.Run,
		body: func(o adminapi.Outcome, res batch.Summary) any {
			return adminapi.LifecycleResponse{Outcome: o, Deleted: res.Succeeded, Failed: res.Failed}
		},
		summary: func(res batch.Summary) (int, string) {
			return res.Succeeded, fmt.Sprintf("expired %d objects", res.Succeeded)
		},
	})
}

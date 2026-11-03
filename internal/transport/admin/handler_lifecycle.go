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
// per object when the caller asks; the pass is LifecyclePass.
// -------------------------------------------------------------------------------

package admin

import "net/http"

// handleLifecycle applies every configured lifecycle rule once and reports what
// it removed.
func (h *Handler) handleLifecycle(w http.ResponseWriter, r *http.Request) {
	h.servePass(w, r, LifecyclePass(h.d.Ops))
}

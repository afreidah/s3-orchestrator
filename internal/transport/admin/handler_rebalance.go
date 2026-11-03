// -------------------------------------------------------------------------------
// Admin API - Rebalance
//
// Author: Alex Freidah
//
// On-demand rebalance trigger. The pass itself is RebalancePass, shared with
// the dashboard.
// -------------------------------------------------------------------------------

package admin

import "net/http"

// handleRebalance triggers one rebalance cycle.
func (h *Handler) handleRebalance(w http.ResponseWriter, r *http.Request) {
	h.servePass(w, r, RebalancePass(h.d.Ops))
}

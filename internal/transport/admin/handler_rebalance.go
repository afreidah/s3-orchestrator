// -------------------------------------------------------------------------------
// Admin API - On-Demand Rebalance Control
//
// Author: Alex Freidah
//
// POST /admin/api/rebalance triggers a one-shot rebalance pass so operators
// can converge object distribution from the CLI without waiting for the
// scheduled worker. A pass can take minutes, so it streams a line per move
// when the caller asks; the cycle itself lives in internal/ops.
// -------------------------------------------------------------------------------

package admin

import (
	"fmt"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// handleRebalance triggers one rebalance cycle. A streamed step names the
// object and the backends it travelled between; moves run concurrently, so
// each line is emitted on completion rather than bracketed.
func (h *Handler) handleRebalance(w http.ResponseWriter, r *http.Request) {
	h.servePass(w, r, passEndpoint[batch.Summary]{
		op:      "rebalance",
		verb:    "moving",
		failMsg: "rebalance failed",
		run:     h.rebalance.Run,
		body: func(o adminapi.Outcome, res batch.Summary) any {
			return adminapi.RebalanceResponse{Outcome: o, Moved: res.Succeeded}
		},
		summary: func(res batch.Summary) (int, string) {
			return res.Succeeded, fmt.Sprintf("moved %d objects", res.Succeeded)
		},
	})
}

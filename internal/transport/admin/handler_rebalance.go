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

	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// handleRebalance triggers one rebalance cycle. Streams per-move NDJSON
// progress when the client accepts the stream content type; otherwise returns a
// single JSON result.
func (h *Handler) handleRebalance(w http.ResponseWriter, r *http.Request) {
	if acceptsStream(r) {
		h.streamRebalance(w, r)
		return
	}

	res, err := h.rebalance.Run(r.Context(), nil)
	if reason, skipped := skipReason(err); skipped {
		httputil.WriteJSON(w, http.StatusOK, adminapi.RebalanceResponse{Status: statusSkipped, Reason: reason})
		return
	}
	if err != nil {
		h.internalError(r.Context(), w, "rebalance failed", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.RebalanceResponse{Status: statusOK, Moved: res.Moved})
}

// streamRebalance runs a rebalance as an NDJSON step stream, one line per move
// naming the object and the backends it travelled between. Moves run
// concurrently, so each line is emitted on completion rather than bracketed.
func (h *Handler) streamRebalance(w http.ResponseWriter, r *http.Request) {
	h.streamSteps(w, "rebalance", "moving", false, func(obs progress.Observer) (stepResult, error) {
		res, err := h.rebalance.Run(r.Context(), obs)
		if reason, skipped := skipReason(err); skipped {
			return stepResult{Skipped: reason}, nil
		}
		if err != nil {
			return stepResult{}, err
		}
		return stepResult{
			Processed: res.Moved,
			Summary:   fmt.Sprintf("moved %d objects", res.Moved),
			Fields:    map[string]any{"moved": res.Moved},
		}, nil
	})
}

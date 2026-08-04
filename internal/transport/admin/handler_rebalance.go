// -------------------------------------------------------------------------------
// Admin API - On-Demand Rebalance Control
//
// Author: Alex Freidah
//
// POST /admin/api/rebalance triggers a one-shot rebalance pass so operators
// can converge object distribution from the CLI without waiting for the
// scheduled worker. Mirrors the dashboard's manual rebalance: it runs the
// configured strategy, falling back to spread-with-defaults when rebalance
// was never configured. A pass can take minutes, so it streams a line per move
// when the caller asks. Exposed as a typed method so UI handlers and tests can
// invoke it without parsing JSON.
// -------------------------------------------------------------------------------

package admin

import (
	"cmp"
	"context"
	"fmt"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// rebalance run defaults applied when the configured value is the zero value,
// matching the dashboard's manual-rebalance behaviour so a never-configured
// rebalance still does something sensible.
const (
	defaultRebalanceStrategy    = "spread"
	defaultRebalanceBatchSize   = 100
	defaultRebalanceThreshold   = 0.1
	defaultRebalanceConcurrency = 5
)

// Rebalance runs one rebalance cycle synchronously and returns the number of
// objects moved. observer, when non-nil, receives a step per move so a
// streaming caller can report progress. Reports a skip, rather than a zero move
// count, when the rebalancer is not wired or the cycle declined to plan any
// moves. Applies the same defaults the dashboard does so a manual run works
// even when rebalance was never configured. Exposed for callers that need the
// outcome as a Go value rather than JSON.
func (h *Handler) Rebalance(ctx context.Context, observer progress.Observer) (adminapi.RebalanceResponse, error) {
	if h.rebalancer == nil {
		return rebalanceSkipped("rebalancer not available"), nil
	}

	var runCfg config.RebalanceConfig
	if cfg := h.rebalancer.Config(); cfg != nil {
		runCfg = *cfg
	}
	runCfg.Strategy = cmp.Or(runCfg.Strategy, defaultRebalanceStrategy)
	runCfg.BatchSize = cmp.Or(runCfg.BatchSize, defaultRebalanceBatchSize)
	runCfg.Threshold = cmp.Or(runCfg.Threshold, defaultRebalanceThreshold)
	runCfg.Concurrency = cmp.Or(runCfg.Concurrency, defaultRebalanceConcurrency)

	sum, err := h.rebalancer.Rebalance(ctx, runCfg, observer)
	if err != nil {
		return adminapi.RebalanceResponse{}, err
	}
	if sum.SkipReason != "" {
		return rebalanceSkipped(sum.SkipReason), nil
	}

	if mErr := h.runtimeOps.UpdateQuotaMetrics(ctx); mErr != nil {
		h.log.WarnContext(ctx, "failed to update quota metrics after rebalance", "error", mErr)
	}
	return adminapi.RebalanceResponse{Status: "ok", Moved: sum.Succeeded}, nil
}

// rebalanceSkipped reports a cycle that did no work, so a caller can tell it
// apart from one that ran and found nothing to move.
func rebalanceSkipped(reason string) adminapi.RebalanceResponse {
	return adminapi.RebalanceResponse{Status: "skipped", Reason: reason}
}

// handleRebalance triggers one rebalance cycle. Streams per-move NDJSON
// progress when the client accepts the stream content type; otherwise returns a
// single JSON result.
func (h *Handler) handleRebalance(w http.ResponseWriter, r *http.Request) {
	if acceptsStream(r) {
		h.streamRebalance(w, r)
		return
	}

	res, err := h.Rebalance(r.Context(), nil)
	if err != nil {
		h.internalError(r.Context(), w, "rebalance failed", err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, res)
}

// streamRebalance runs a rebalance as an NDJSON step stream, one line per move
// naming the object and the backends it travelled between. Moves run
// concurrently, so each line is emitted on completion rather than bracketed.
func (h *Handler) streamRebalance(w http.ResponseWriter, r *http.Request) {
	h.streamSteps(w, "rebalance", "moving", false, func(obs progress.Observer) (stepResult, error) {
		res, err := h.Rebalance(r.Context(), obs)
		if err != nil {
			return stepResult{}, err
		}
		if res.Status == "skipped" {
			return stepResult{Skipped: res.Reason}, nil
		}
		return stepResult{
			Processed: res.Moved,
			Summary:   fmt.Sprintf("moved %d objects", res.Moved),
			Fields:    map[string]any{"moved": res.Moved},
		}, nil
	})
}

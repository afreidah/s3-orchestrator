// -------------------------------------------------------------------------------
// Admin API - On-Demand Rebalance Control
//
// Author: Alex Freidah
//
// POST /admin/api/rebalance triggers a one-shot rebalance pass so operators
// can converge object distribution from the CLI without waiting for the
// scheduled worker. Mirrors the dashboard's manual rebalance: it runs the
// configured strategy, falling back to spread-with-defaults when rebalance
// was never configured. Exposed as a typed method so UI handlers and tests
// can invoke it without parsing JSON.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/config"
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
// objects moved. Skips when the rebalancer worker is not wired. Applies the
// same defaults the dashboard does so a manual run works even when rebalance
// was never configured. Exposed for callers that need the outcome as a Go
// value rather than JSON.
func (h *Handler) Rebalance(ctx context.Context) (adminapi.RebalanceResponse, error) {
	if h.rebalancer == nil {
		return adminapi.RebalanceResponse{Status: "skipped", Reason: "rebalancer not available"}, nil
	}

	var runCfg config.RebalanceConfig
	if cfg := h.rebalancer.Config(); cfg != nil {
		runCfg = *cfg
	}
	if runCfg.Strategy == "" {
		runCfg.Strategy = defaultRebalanceStrategy
	}
	if runCfg.BatchSize == 0 {
		runCfg.BatchSize = defaultRebalanceBatchSize
	}
	if runCfg.Threshold == 0 {
		runCfg.Threshold = defaultRebalanceThreshold
	}
	if runCfg.Concurrency == 0 {
		runCfg.Concurrency = defaultRebalanceConcurrency
	}

	sum, err := h.rebalancer.Rebalance(ctx, runCfg)
	if err != nil {
		return adminapi.RebalanceResponse{}, err
	}

	if mErr := h.runtimeOps.UpdateQuotaMetrics(ctx); mErr != nil {
		h.log.WarnContext(ctx, "failed to update quota metrics after rebalance", "error", mErr)
	}
	return adminapi.RebalanceResponse{Status: "ok", Moved: sum.Succeeded}, nil
}

// handleRebalance triggers one rebalance cycle and returns the move count.
func (h *Handler) handleRebalance(w http.ResponseWriter, r *http.Request) {
	res, err := h.Rebalance(r.Context())
	if err != nil {
		h.internalError(r.Context(), w, "rebalance failed", err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, res)
}

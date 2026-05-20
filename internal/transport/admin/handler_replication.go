// -------------------------------------------------------------------------------
// Admin API - Replication and Over-Replication Control
//
// Author: Alex Freidah
//
// /admin/api/replicate triggers a one-shot replication pass to fill in
// under-replicated objects; the over-replication endpoints expose count
// + cleanup so operators can drive excess-copy removal from outside the
// scheduled cleaner. Replicate is also exposed as a typed method so UI
// handlers and tests can invoke it without parsing JSON.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"net/http"
	"strconv"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// ReplicateResult is the outcome of a one-shot replication cycle.
type ReplicateResult struct {
	Status        string // "ok" or "skipped"
	Reason        string // populated when Status is "skipped"
	CopiesCreated int
}

// Replicate runs one replication cycle synchronously and returns the
// resulting counts. Skips when replication is unconfigured or factor <= 1.
// Refreshes quota metrics on success. Exposed for callers (UI, tests) that
// need the counts back as Go values rather than JSON.
func (h *Handler) Replicate(ctx context.Context) (ReplicateResult, error) {
	rcfg := h.replicator.Config()
	if rcfg == nil || rcfg.Factor <= 1 {
		return ReplicateResult{Status: "skipped", Reason: "replication not configured or factor <= 1"}, nil
	}

	created, err := h.replicator.Replicate(ctx, *rcfg)
	if err != nil {
		return ReplicateResult{}, err
	}

	if mErr := h.backendOps.UpdateQuotaMetrics(ctx); mErr != nil {
		h.log.WarnContext(ctx, "failed to update quota metrics after replicate", "error", mErr)
	}

	return ReplicateResult{Status: "ok", CopiesCreated: created}, nil
}

// handleReplicate triggers one replication cycle.
func (h *Handler) handleReplicate(w http.ResponseWriter, r *http.Request) {
	res, err := h.Replicate(r.Context())
	if err != nil {
		h.internalError(r.Context(), w, "replication failed", err)
		return
	}

	if res.Status == "skipped" {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{
			"status":         "skipped",
			"copies_created": 0,
			"reason":         res.Reason,
		})
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "copies_created": res.CopiesCreated})
}

// handleOverReplicationStatus returns the count of over-replicated objects.
func (h *Handler) handleOverReplicationStatus(w http.ResponseWriter, r *http.Request) {
	rcfg := h.overRep.Config()
	if rcfg == nil || rcfg.Factor <= 1 {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{
			"factor":  0,
			"pending": 0,
			"status":  "replication not configured",
		})
		return
	}

	count, err := h.overRep.CountPending(r.Context(), rcfg.Factor)
	if err != nil {
		h.internalError(r.Context(), w, "failed to count over-replicated objects", err)
		return
	}

	telemetry.OverReplicationPending.Set(float64(count))
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"factor":  rcfg.Factor,
		"pending": count,
	})
}

// handleOverReplicationClean triggers an immediate over-replication cleanup pass.
func (h *Handler) handleOverReplicationClean(w http.ResponseWriter, r *http.Request) {
	rcfg := h.overRep.Config()
	if rcfg == nil || rcfg.Factor <= 1 {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{
			"status":         "skipped",
			"copies_removed": 0,
			"reason":         "replication not configured or factor <= 1",
		})
		return
	}

	// Allow callers to override batch size via query parameter.
	cfg := *rcfg
	if bs := r.URL.Query().Get("batch_size"); bs != "" {
		if n, err := strconv.Atoi(bs); err == nil && n > 0 {
			if n > 10000 {
				n = 10000
			}
			cfg.BatchSize = n
		}
	}

	removed, err := h.overRep.Clean(r.Context(), cfg)
	if err != nil {
		h.internalError(r.Context(), w, "over-replication cleanup failed", err)
		return
	}

	if err := h.backendOps.UpdateQuotaMetrics(r.Context()); err != nil {
		h.log.WarnContext(r.Context(), "failed to update quota metrics after admin over-replication cleanup", "error", err)
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "copies_removed": removed})
}

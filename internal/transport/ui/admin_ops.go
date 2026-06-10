// -------------------------------------------------------------------------------
// UI Handler - Async Admin Operations (Rebalance, Clean-Excess, Sync)
//
// Author: Alex Freidah
//
// Worker-driven admin operations the dashboard exposes. Each long-running
// op returns 202 Accepted immediately while the work runs in a goroutine
// tracked by asyncOps; clients poll the matching /status endpoint to
// surface progress and final results. writeAsyncOpStatus is the shared
// JSON shape every status endpoint emits so the dashboard can render a
// single status pane regardless of which op is running. Sync is the
// odd one out - it is synchronous because backend sync is fast enough
// to block the request and benefits from inline error reporting.
// -------------------------------------------------------------------------------

package ui

import (
	"context"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// handleAPIRebalance triggers an on-demand rebalance in the background.
// Returns 202 Accepted immediately; poll /api/rebalance/status for results.
func (h *Handler) handleAPIRebalance(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	if !h.asyncOps.TryStart("rebalance") {
		httputil.WriteJSON(w, http.StatusConflict, map[string]string{"error": "rebalance already running"})
		return
	}

	rebalCfg := h.rebalancer.Config()
	if rebalCfg == nil {
		rebalCfg = &h.cfg.Load().Rebalance
	}
	runCfg := *rebalCfg
	if runCfg.Strategy == "" {
		runCfg.Strategy = "spread"
	}
	if runCfg.BatchSize == 0 {
		runCfg.BatchSize = 100
	}
	if runCfg.Threshold == 0 {
		runCfg.Threshold = 0.1
	}
	if runCfg.Concurrency == 0 {
		runCfg.Concurrency = 5
	}

	go func() {
		ctx := context.Background()
		moved, err := h.rebalancer.Rebalance(ctx, runCfg)
		if err != nil {
			h.log.ErrorContext(ctx, "rebalance failed", "error", err)
			h.asyncOps.Complete("rebalance", &asyncResult{Error: "rebalance failed"})
			return
		}
		h.log.InfoContext(ctx, "manual rebalance completed", "moved", moved)
		h.asyncOps.Complete("rebalance", &asyncResult{OK: true, Count: moved})
	}()

	httputil.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// handleAPIRebalanceStatus returns the status of a running or completed rebalance.
func (h *Handler) handleAPIRebalanceStatus(w http.ResponseWriter, _ *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set(headerContentType, contentTypeJSON)
	result, running := h.asyncOps.Status("rebalance")
	writeAsyncOpStatus(w, result, running, "moved")
}

// writeAsyncOpStatus encodes the JSON response for any async-op status
// endpoint. countKey is the field name used to surface the operation's
// scalar result (for example "moved" for rebalance, "removed" for the
// over-replication cleaner) inside the done payload. Note: caller is
// expected to have already set the JSON content-type header so this
// helper can also be invoked from non-Handler contexts that pre-stage
// headers (e.g. existing admin_actions.go status endpoints).
func writeAsyncOpStatus(w http.ResponseWriter, result *asyncResult, running bool, countKey string) {
	switch {
	case running:
		httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "running"})
	case result == nil:
		httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "idle"})
	case result.Error != "":
		httputil.WriteJSON(w, http.StatusOK, map[string]any{"status": "error", "error": result.Error})
	default:
		httputil.WriteJSON(w, http.StatusOK, map[string]any{"status": "done", "ok": true, countKey: result.Count})
	}
}

// handleAPICleanExcess triggers an on-demand over-replication cleanup in the
// background. Returns 202 Accepted immediately; poll /api/clean-excess/status.
func (h *Handler) handleAPICleanExcess(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	rcfg := h.overRep.Config()
	if rcfg == nil {
		rcfg = &h.cfg.Load().Replication
	}
	if rcfg.Factor <= 1 {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": 0, "reason": "replication factor <= 1"})
		return
	}

	if !h.asyncOps.TryStart(opCleanExcess) {
		httputil.WriteJSON(w, http.StatusConflict, map[string]string{"error": "cleanup already running"})
		return
	}

	cfg := *rcfg
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 5
	}

	go func() {
		ctx := context.Background()
		removed, err := h.overRep.Clean(ctx, cfg, nil)
		if err != nil {
			h.log.ErrorContext(ctx, "over-replication cleanup failed", "error", err)
			h.asyncOps.Complete(opCleanExcess, &asyncResult{Error: "cleanup failed"})
			return
		}
		h.log.InfoContext(ctx, "manual over-replication cleanup completed", "removed", removed)
		h.asyncOps.Complete(opCleanExcess, &asyncResult{OK: true, Count: removed})
	}()

	httputil.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// handleAPICleanExcessStatus returns the status of a running or completed cleanup.
func (h *Handler) handleAPICleanExcessStatus(w http.ResponseWriter, _ *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set(headerContentType, contentTypeJSON)
	result, running := h.asyncOps.Status(opCleanExcess)
	writeAsyncOpStatus(w, result, running, "removed")
}

// handleAPISync triggers a backend sync to import pre-existing objects.
func (h *Handler) handleAPISync(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	var req struct {
		Backend string `json:"backend"`
		Bucket  string `json:"bucket"`
	}
	if !httputil.DecodeJSONBody(w, r, &req, 1<<20) {
		return
	}
	if req.Backend == "" || req.Bucket == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "backend and bucket are required")
		return
	}

	if !h.validBackend(req.Backend) {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid backend or bucket")
		return
	}

	if !h.validBucketPrefix(req.Bucket + "/") {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid backend or bucket")
		return
	}

	cfg := h.cfg.Load()
	bucketNames := make([]string, len(cfg.Buckets))
	for i, b := range cfg.Buckets {
		bucketNames[i] = b.Name
	}

	imported, skipped, err := h.backendOps.SyncBackend(r.Context(), req.Backend, req.Bucket, bucketNames)
	if err != nil {
		h.log.ErrorContext(r.Context(), "sync failed", "backend", req.Backend, "bucket", req.Bucket, "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "sync failed")
		return
	}

	h.log.InfoContext(r.Context(), "manual sync completed", "backend", req.Backend, "bucket", req.Bucket,
		"imported", imported, "skipped", skipped)
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": imported, "skipped": skipped})
}

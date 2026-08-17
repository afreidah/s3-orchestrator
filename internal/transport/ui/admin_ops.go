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

	if !h.asyncOps.TryStart(opRebalance) {
		httputil.WriteJSON(w, http.StatusConflict, map[string]string{"error": "rebalance already running"})
		return
	}

	go func() {
		ctx := context.Background()
		res, err := h.rebalance.Run(ctx, nil)
		if reason, skipped := skipReason(err); skipped {
			h.asyncOps.Complete(opRebalance, &asyncResult{OK: true, Skipped: reason})
			return
		}
		if err != nil {
			h.log.ErrorContext(ctx, "rebalance failed", "error", err)
			h.asyncOps.Complete(opRebalance, &asyncResult{Error: "rebalance failed"})
			return
		}
		h.asyncOps.Complete(opRebalance, &asyncResult{OK: true, Count: res.Moved})
	}()

	httputil.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// handleAPIRebalanceStatus returns the status of a running or completed rebalance.
func (h *Handler) handleAPIRebalanceStatus(w http.ResponseWriter, _ *http.Request) {
	h.writeAdminActionStatus(w, opRebalance, "moved")
}

// handleAPICleanExcess triggers an on-demand over-replication cleanup in the
// background. Returns 202 Accepted immediately; poll /api/clean-excess/status.
func (h *Handler) handleAPICleanExcess(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	if !h.asyncOps.TryStart(opCleanExcess) {
		httputil.WriteJSON(w, http.StatusConflict, map[string]string{"error": "cleanup already running"})
		return
	}

	go func() {
		ctx := context.Background()
		res, err := h.replication.CleanExcess(ctx, 0, nil)
		if reason, skipped := skipReason(err); skipped {
			h.asyncOps.Complete(opCleanExcess, &asyncResult{OK: true, Skipped: reason})
			return
		}
		if err != nil {
			h.log.ErrorContext(ctx, "over-replication cleanup failed", "error", err)
			h.asyncOps.Complete(opCleanExcess, &asyncResult{Error: "cleanup failed"})
			return
		}
		h.asyncOps.Complete(opCleanExcess, &asyncResult{
			OK:    true,
			Count: res.CopiesRemoved,
			Extra: map[string]any{"failed": res.Failed},
		})
	}()

	httputil.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// handleAPICleanExcessStatus returns the status of a running or completed cleanup.
func (h *Handler) handleAPICleanExcessStatus(w http.ResponseWriter, _ *http.Request) {
	h.writeAdminActionStatus(w, opCleanExcess, "removed")
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

	imported, skipped, err := h.syncOps.SyncBackend(r.Context(), req.Backend, req.Bucket, bucketNames)
	if err != nil {
		h.log.ErrorContext(r.Context(), "sync failed", "backend", req.Backend, "bucket", req.Bucket, "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "sync failed")
		return
	}

	h.log.InfoContext(r.Context(), "manual sync completed", "backend", req.Backend, "bucket", req.Bucket,
		"imported", imported, "skipped", skipped)
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": imported, "skipped": skipped})
}

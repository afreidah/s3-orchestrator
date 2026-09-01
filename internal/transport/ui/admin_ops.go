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

	h.startAdminAction(w, r, h.rebalanceOp())
}

// rebalanceStatus reports a rebalance cycle. The dashboard keys on moved.
type rebalanceStatus struct {
	adminActionState
	Moved int `json:"moved"`
}

// rebalanceOp is the rebalance action, shared by its trigger and its poll.
func (h *Handler) rebalanceOp() adminActionOp[rebalanceStatus] {
	return adminActionOp[rebalanceStatus]{
		name: opRebalance,
		run: func(ctx context.Context) (adminActionCounts, string, error) {
			res, err := h.rebalance.Run(ctx, nil)
			if reason, skipped := skipReason(err); skipped {
				return adminActionCounts{}, reason, nil
			}
			if err != nil {
				return adminActionCounts{}, "", err
			}
			return adminActionCounts{Count: res.Moved}, "", nil
		},
		render: func(s adminActionState, c adminActionCounts) rebalanceStatus {
			return rebalanceStatus{adminActionState: s, Moved: c.Count}
		},
	}
}

// handleAPIRebalanceStatus returns the status of a running or completed rebalance.
func (h *Handler) handleAPIRebalanceStatus(w http.ResponseWriter, _ *http.Request) {
	h.writeAdminActionStatus(w, h.rebalanceOp())
}

// handleAPICleanExcess triggers an on-demand over-replication cleanup in the
// background. Returns 202 Accepted immediately; poll /api/clean-excess/status.
func (h *Handler) handleAPICleanExcess(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	h.startAdminAction(w, r, h.cleanExcessOp())
}

// cleanExcessStatus reports an over-replication cleanup. The dashboard keys on
// removed.
type cleanExcessStatus struct {
	adminActionState
	Removed int `json:"removed"`
	Failed  int `json:"failed"`
}

// cleanExcessOp is the clean-excess action, shared by its trigger and its poll.
func (h *Handler) cleanExcessOp() adminActionOp[cleanExcessStatus] {
	return adminActionOp[cleanExcessStatus]{
		name: opCleanExcess,
		run: func(ctx context.Context) (adminActionCounts, string, error) {
			res, err := h.replication.CleanExcess(ctx, 0, nil)
			if reason, skipped := skipReason(err); skipped {
				return adminActionCounts{}, reason, nil
			}
			if err != nil {
				return adminActionCounts{}, "", err
			}
			return adminActionCounts{Count: res.CopiesRemoved, Failed: res.Failed}, "", nil
		},
		render: func(s adminActionState, c adminActionCounts) cleanExcessStatus {
			return cleanExcessStatus{adminActionState: s, Removed: c.Count, Failed: c.Failed}
		},
	}
}

// handleAPICleanExcessStatus returns the status of a running or completed cleanup.
func (h *Handler) handleAPICleanExcessStatus(w http.ResponseWriter, _ *http.Request) {
	h.writeAdminActionStatus(w, h.cleanExcessOp())
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

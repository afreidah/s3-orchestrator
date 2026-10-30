// -------------------------------------------------------------------------------
// UI Handler - Async Admin Operations (Rebalance, Clean-Excess, Sync)
//
// Author: Alex Freidah
//
// Worker-driven admin operations the dashboard exposes. Each long-running
// op returns 202 Accepted immediately while the work runs in a goroutine
// tracked by asyncOps; clients poll the matching /status endpoint to
// surface progress and final results. writeAdminActionStatus is the shared
// JSON shape every status endpoint emits so the dashboard can render a
// single status pane regardless of which op is running. Sync is the
// odd one out - it is synchronous because backend sync is fast enough
// to block the request and benefits from inline error reporting.
// -------------------------------------------------------------------------------

package ui

import (
	"context"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// REBALANCE
// -------------------------------------------------------------------------

// rebalanceStatus reports a rebalance cycle. The dashboard keys on moved.
type rebalanceStatus struct {
	adminActionState
	Moved int `json:"moved"`
}

// rebalanceOp runs an on-demand rebalance.
func (h *Handler) rebalanceOp(_ *http.Request) adminActionOp[rebalanceStatus] {
	return adminActionOp[rebalanceStatus]{
		name: opRebalance,
		run: func(ctx context.Context) (adminActionCounts, error) {
			res, err := h.d.Rebalance.Run(ctx, nil)
			if err != nil {
				return adminActionCounts{}, err
			}
			return adminActionCounts{Count: res.Succeeded}, nil
		},
		render: func(s adminActionState, c adminActionCounts) rebalanceStatus {
			return rebalanceStatus{adminActionState: s, Moved: c.Count}
		},
	}
}

// -------------------------------------------------------------------------
// OVER-REPLICATION CLEANUP
// -------------------------------------------------------------------------

// cleanExcessStatus reports an over-replication cleanup. The dashboard keys on
// removed.
type cleanExcessStatus struct {
	adminActionState
	Removed int `json:"removed"`
	Failed  int `json:"failed"`
}

// cleanExcessOp runs an on-demand over-replication cleanup.
func (h *Handler) cleanExcessOp(_ *http.Request) adminActionOp[cleanExcessStatus] {
	return adminActionOp[cleanExcessStatus]{
		name: opCleanExcess,
		run: func(ctx context.Context) (adminActionCounts, error) {
			res, err := h.d.Replication.CleanExcess(ctx, 0, nil)
			if err != nil {
				return adminActionCounts{}, err
			}
			return adminActionCounts{Count: res.CopiesRemoved, Failed: res.Failed}, nil
		},
		render: func(s adminActionState, c adminActionCounts) cleanExcessStatus {
			return cleanExcessStatus{adminActionState: s, Removed: c.Count, Failed: c.Failed}
		},
	}
}

// handleAPISync triggers a backend sync to import pre-existing objects. It
// records objects into one bucket from one backend's listing, so it needs write
// on the bucket and admin-maintain on that backend.
func (h *Handler) handleAPISync(w http.ResponseWriter, r *http.Request) {
	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	var req struct {
		Backend string `json:"backend"`
		Bucket  string `json:"bucket"`
	}
	if !httputil.DecodeJSONBody(w, r, &req, httputil.MaxJSONBody) {
		return
	}
	if req.Backend == "" || req.Bucket == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "backend and bucket are required")
		return
	}
	if !h.validBackend(req.Backend) || !h.d.Buckets.HasPrefix(req.Bucket+"/") {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid backend or bucket")
		return
	}

	if !h.authorizeKey(w, r, req.Bucket+"/", core.PermWrite) {
		return
	}
	backend := core.Resource{Kind: core.ResourceBackend, Name: req.Backend}
	if !userFrom(r.Context()).CanAdmin(backend, core.PermAdminMaintain) {
		h.refuse(w, r, "/api/sync", "grant does not carry the permission", backend.String())
		return
	}

	imported, skipped, err := h.d.Sync.SyncBackend(r.Context(), req.Backend, h.d.Buckets.Names())
	if err != nil {
		httputil.InternalError(r.Context(), h.log, w, "sync failed", err, "backend", req.Backend, "bucket", req.Bucket)
		return
	}

	h.log.InfoContext(r.Context(), "manual sync completed", "backend", req.Backend, "bucket", req.Bucket,
		"imported", imported, "skipped", skipped)
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": imported, "skipped": skipped})
}

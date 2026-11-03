// -------------------------------------------------------------------------------
// UI Handler - Backend Sync
//
// Author: Alex Freidah
//
// Imports a backend's pre-existing objects into one bucket. Unlike the
// dashboard's other actions it runs synchronously, because a sync is fast
// enough to block the request and benefits from inline error reporting.
// -------------------------------------------------------------------------------

package ui

import (
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// syncResponse answers a backend sync with how many objects it imported and
// how many it skipped because the ledger already tracked them.
type syncResponse struct {
	OK       bool `json:"ok"`
	Imported int  `json:"imported"`
	Skipped  int  `json:"skipped"`
}

// handleAPISync triggers a backend sync to import pre-existing objects. It
// records objects into one bucket from one backend's listing, so it needs write
// on the bucket and admin-maintain on that backend.
func (h *Handler) handleAPISync(w http.ResponseWriter, r *http.Request) {
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
	prefix := internalkey.Prefix(req.Bucket)
	if !h.d.Config.Load().HasBackend(req.Backend) || !h.d.Buckets.HasPrefix(prefix) {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid backend or bucket")
		return
	}

	if !h.authorizeKey(w, r, prefix, core.PermWrite) {
		return
	}
	backend := core.Resource{Kind: core.ResourceBackend, Name: req.Backend}
	if user := userFrom(r.Context()); !user.CanAdmin(backend, core.PermAdminMaintain) {
		h.refuse(w, r, &auth.Denial{
			User: user, Operation: routeSync, Resource: backend,
			Required: core.PermAdminMaintain, Reason: auth.ReasonPermission,
		})
		return
	}

	imported, skipped, err := h.d.Sync.SyncBackend(r.Context(), req.Backend, prefix, h.d.Buckets.Names())
	if err != nil {
		httputil.InternalError(r.Context(), h.log, w, "sync failed", err, "backend", req.Backend, "bucket", req.Bucket)
		return
	}

	h.log.InfoContext(r.Context(), "manual sync completed", "backend", req.Backend, "bucket", req.Bucket,
		"imported", imported, "skipped", skipped)
	httputil.WriteJSON(w, http.StatusOK, syncResponse{OK: true, Imported: imported, Skipped: skipped})
}

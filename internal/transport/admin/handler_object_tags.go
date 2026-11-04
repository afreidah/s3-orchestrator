// -------------------------------------------------------------------------------
// Admin API - Object Tags
//
// Author: Alex Freidah
//
// An object's tag set as an operator reaches it: read what a key carries, set
// the whole set, or clear it. JSON rather than the Tagging XML the S3 endpoints
// exchange, because this is the control plane and its callers are adminctl,
// the dashboard and the TUI.
//
// Validation, the key lock and the refusal of a key holding no copies all live
// in the store, reached through the shared ops layer, so this file owns only
// the wire shape.
// -------------------------------------------------------------------------------

package admin

import (
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// handleGetObjectTags returns one object's tag set.
func (h *Handler) handleGetObjectTags(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue(adminapi.ParamKey)

	tags, err := h.d.Ops.Objects.Tags(r.Context(), key)
	if err != nil {
		httputil.WriteOpError(w, r, h.log, err, "failed to read object tags", "key", key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectTagsResponse{Tags: adminapi.ObjectTagsFrom(tags)})
}

// handlePutObjectTags replaces one object's whole tag set. An empty list
// leaves the object untagged, which is the same outcome as a delete.
func (h *Handler) handlePutObjectTags(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue(adminapi.ParamKey)

	var req adminapi.ObjectTagsRequest
	if !httputil.DecodeJSONBody(w, r, &req, maxSmallBody) {
		return
	}

	tags := adminapi.CoreTags(req.Tags)
	if err := h.d.Ops.Objects.SetTags(r.Context(), key, tags); err != nil {
		httputil.WriteOpError(w, r, h.log, err, msgWriteTagsFailed, "key", key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectTagsResponse{Tags: adminapi.ObjectTagsFrom(tags)})
}

// handleDeleteObjectTags clears one object's tag set.
func (h *Handler) handleDeleteObjectTags(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue(adminapi.ParamKey)

	if err := h.d.Ops.Objects.DeleteTags(r.Context(), key); err != nil {
		httputil.WriteOpError(w, r, h.log, err, msgWriteTagsFailed, "key", key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectTagsResponse{Tags: []adminapi.ObjectTag{}})
}

// msgWriteTagsFailed is what a caller is told when a tag write fails for a
// reason other than the request itself.
const msgWriteTagsFailed = "failed to write object tags"

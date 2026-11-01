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
// the wire shape and the status codes.
// -------------------------------------------------------------------------------

package admin

import (
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// handleGetObjectTags returns one object's tag set.
func (h *Handler) handleGetObjectTags(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue(paramKey)

	tags, err := h.d.Objects.Tags(r.Context(), key)
	if err != nil {
		httputil.WriteObjectError(w, r, h.log, err, "failed to read object tags", key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectTagsResponse{Tags: adminapi.ObjectTagsFrom(tags)})
}

// handlePutObjectTags replaces one object's whole tag set. An empty list
// leaves the object untagged, which is the same outcome as a delete.
func (h *Handler) handlePutObjectTags(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue(paramKey)

	var req adminapi.ObjectTagsRequest
	if !httputil.DecodeJSONBody(w, r, &req, maxSmallBody) {
		return
	}

	tags := adminapi.CoreTags(req.Tags)
	if err := h.d.Objects.SetTags(r.Context(), key, tags); err != nil {
		h.writeObjectTagError(w, r, err, key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectTagsResponse{Tags: adminapi.ObjectTagsFrom(tags)})
}

// handleDeleteObjectTags clears one object's tag set.
func (h *Handler) handleDeleteObjectTags(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue(paramKey)

	if err := h.d.Objects.DeleteTags(r.Context(), key); err != nil {
		h.writeObjectTagError(w, r, err, key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectTagsResponse{Tags: []adminapi.ObjectTag{}})
}

// writeObjectTagError renders a tag-set failure, mapping the validation
// refusals to 400 before falling back to the shared object-error mapping.
//
// The sentinels carry the offending measurement in their message, so the
// caller sees which limit it exceeded rather than a bare "invalid".
func (h *Handler) writeObjectTagError(w http.ResponseWriter, r *http.Request, err error, key string) {
	if core.IsTagValidationError(err) {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	httputil.WriteObjectError(w, r, h.log, err, "failed to write object tags", key)
}

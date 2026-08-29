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
	"encoding/json"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// maxObjectTagsBody caps the tag-set request body. Ten tags at the maximum key
// and value lengths is a few kilobytes, so this sits far above any legal
// request while still bounding what an attacker can send.
const maxObjectTagsBody = 64 << 10

// handleGetObjectTags returns one object's tag set.
func (h *Handler) handleGetObjectTags(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	tags, err := h.objects.Tags(r.Context(), key)
	if err != nil {
		h.writeObjectError(w, r, err, "failed to read object tags", key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectTagsResponse{Tags: apiTagsFrom(tags)})
}

// handlePutObjectTags replaces one object's whole tag set. An empty list
// leaves the object untagged, which is the same outcome as a delete.
func (h *Handler) handlePutObjectTags(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	var req adminapi.ObjectTagsRequest
	body := http.MaxBytesReader(w, r.Body, maxObjectTagsBody)
	defer body.Close()
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid tag set: "+err.Error())
		return
	}

	if err := h.objects.SetTags(r.Context(), key, coreTagsFrom(req.Tags)); err != nil {
		h.writeObjectTagError(w, r, err, key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectTagsResponse(req))
}

// handleDeleteObjectTags clears one object's tag set.
func (h *Handler) handleDeleteObjectTags(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	if err := h.objects.DeleteTags(r.Context(), key); err != nil {
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
	h.writeObjectError(w, r, err, "failed to write object tags", key)
}

// apiTagsFrom converts stored tags to the wire shape, always returning a
// non-nil slice so an untagged object serialises as [] rather than null.
func apiTagsFrom(tags []core.Tag) []adminapi.ObjectTag {
	out := make([]adminapi.ObjectTag, len(tags))
	for i, t := range tags {
		out[i] = adminapi.ObjectTag{Key: t.Key, Value: t.Value}
	}
	return out
}

// coreTagsFrom converts the wire shape to stored tags.
func coreTagsFrom(tags []adminapi.ObjectTag) []core.Tag {
	out := make([]core.Tag, len(tags))
	for i, t := range tags {
		out[i] = core.Tag{Key: t.Key, Value: t.Value}
	}
	return out
}

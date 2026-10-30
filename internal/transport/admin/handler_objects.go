// -------------------------------------------------------------------------------
// Admin API - Object Browse, Read and Write
//
// Author: Alex Freidah
//
// The object namespace as an operator sees it from a terminal: browse one
// delimiter-grouped page, stream a single object down, store one, and remove
// either a single key or everything under a prefix. The same operations back
// the dashboard's file browser, so a caller signing with a credential reaches
// exactly what a session holding that credential does.
// -------------------------------------------------------------------------------

package admin

import (
	"log/slog"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// objectKeyPattern is the path suffix carrying an object key. Keys contain
// slashes, so the wildcard is greedy.
const objectKeyPattern = "{key...}"

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// handleListObjects returns one page under the given prefix. An omitted
// delimiter defaults to "/" so browsing is hierarchical; an explicitly empty
// one lists every key under the prefix flat, which is what a caller counting
// or sweeping a subtree needs. Continuation resumes a truncated page.
func (h *Handler) handleListObjects(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get(paramPrefix)

	delimiter := q.Get("delimiter")
	if !q.Has("delimiter") {
		delimiter = "/"
	}

	page, err := h.d.Objects.List(r.Context(), prefix, delimiter, q.Get("continuation"), 0)
	if err != nil {
		httputil.InternalError(r.Context(), h.log, w, "failed to list objects", err, slog.String("prefix", prefix))
		return
	}

	resp := adminapi.ObjectListResponse{
		CommonPrefixes: page.CommonPrefixes,
		Truncated:      page.IsTruncated,
		Next:           page.NextContinuationToken,
	}
	for i := range page.Objects {
		resp.Objects = append(resp.Objects, adminapi.ObjectEntry{
			Key:  page.Objects[i].ObjectKey,
			Size: page.Objects[i].SizeBytes,
		})
	}

	httputil.WriteJSON(w, http.StatusOK, resp)
}

// handleGetObject streams one object to the caller.
func (h *Handler) handleGetObject(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue(paramKey)

	result, err := h.d.Objects.Get(r.Context(), key)
	if err != nil {
		httputil.WriteObjectError(w, r, h.log, err, "failed to read object", key)
		return
	}
	defer result.Body.Close()

	if err := httputil.ServeAttachment(w, key, result.Size, result.Body); err != nil {
		h.log.WarnContext(r.Context(), "object download interrupted", "key", key, "error", err)
	}
}

// handlePutObject stores the request body as one object. The body is the
// object bytes, so a caller uploads a file by streaming it directly rather
// than wrapping it in a form.
func (h *Handler) handlePutObject(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue(paramKey)

	if r.ContentLength <= 0 {
		httputil.WriteJSONError(w, http.StatusLengthRequired, "content-length is required")
		return
	}
	if r.ContentLength > ops.MaxUploadSize {
		httputil.WriteJSONError(w, http.StatusRequestEntityTooLarge, "object exceeds the maximum upload size")
		return
	}

	body := http.MaxBytesReader(w, r.Body, ops.MaxUploadSize)
	defer body.Close()

	etag, err := h.d.Objects.Put(r.Context(), key, body, r.ContentLength, r.Header.Get("Content-Type"))
	if err != nil {
		httputil.WriteObjectError(w, r, h.log, err, "failed to store object", key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectUploadResponse{ETag: etag})
}

// handleDeleteObject removes one object and every copy of it.
func (h *Handler) handleDeleteObject(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue(paramKey)

	if err := h.d.Objects.Delete(r.Context(), key); err != nil {
		httputil.WriteObjectError(w, r, h.log, err, "failed to delete object", key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectDeleteResponse{Deleted: 1})
}

// handleDeletePrefix removes every object under the given prefix. The response
// carries the count, so a caller can tell a no-op from a mass removal, and a
// partial removal is reported as a failure carrying what it did remove.
func (h *Handler) handleDeletePrefix(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get(paramPrefix)

	res, err := h.d.Objects.DeletePrefix(r.Context(), prefix, nil)
	if err != nil {
		httputil.WriteObjectError(w, r, h.log, err, "failed to delete prefix", prefix)
		return
	}

	status := http.StatusOK
	if res.Failed > 0 {
		status = http.StatusInternalServerError
	}
	httputil.WriteJSON(w, status, adminapi.ObjectDeleteResponse{Deleted: res.Deleted, Failed: res.Failed, Total: res.Total})
}

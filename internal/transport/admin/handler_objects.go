// -------------------------------------------------------------------------------
// Admin API - Object Browse, Read and Write
//
// Author: Alex Freidah
//
// The object namespace as an operator sees it from a terminal: browse one
// delimiter-grouped page, stream a single object down, store one, and remove
// either a single key or everything under a prefix. The same operations back
// the dashboard's file browser, so a token-authenticated caller reaches
// exactly what a session-authenticated one does.
// -------------------------------------------------------------------------------

package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"path"
	"strconv"

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
)

// objectKeyPattern is the path suffix carrying an object key. Keys contain
// slashes, so the wildcard is greedy.
const objectKeyPattern = "{key...}"

// handleListObjects returns one page under the given prefix. An omitted
// delimiter defaults to "/" so browsing is hierarchical; an explicitly empty
// one lists every key under the prefix flat, which is what a caller counting
// or sweeping a subtree needs. Continuation resumes a truncated page.
func (h *Handler) handleListObjects(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("prefix")

	delimiter := q.Get("delimiter")
	if !q.Has("delimiter") {
		delimiter = "/"
	}

	page, err := h.objects.List(r.Context(), prefix, delimiter, q.Get("continuation"), 0)
	if err != nil {
		h.internalError(r.Context(), w, "failed to list objects", err, slog.String("prefix", prefix))
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
	key := r.PathValue("key")

	result, err := h.objects.Get(r.Context(), key)
	if err != nil {
		h.writeObjectError(w, r, err, "failed to read object", key)
		return
	}
	defer result.Body.Close()

	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(path.Base(key)))
	w.Header().Set("Content-Type", "application/octet-stream")
	if result.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	}

	if _, err := bufpool.Copy(w, result.Body); err != nil {
		h.log.WarnContext(r.Context(), "object download interrupted", "key", key, "error", err)
	}
}

// handlePutObject stores the request body as one object. The body is the
// object bytes, so a caller uploads a file by streaming it directly rather
// than wrapping it in a form.
func (h *Handler) handlePutObject(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

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

	etag, err := h.objects.Put(r.Context(), key, body, r.ContentLength, r.Header.Get("Content-Type"))
	if err != nil {
		h.writeObjectError(w, r, err, "failed to store object", key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectUploadResponse{ETag: etag})
}

// handleDeleteObject removes one object and every copy of it.
func (h *Handler) handleDeleteObject(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	if err := h.objects.Delete(r.Context(), key); err != nil {
		h.writeObjectError(w, r, err, "failed to delete object", key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectDeleteResponse{Deleted: 1})
}

// handleDeletePrefix removes every object under the given prefix. The response
// carries the count, so a caller can tell a no-op from a mass removal, and a
// partial removal is reported as a failure carrying what it did remove.
func (h *Handler) handleDeletePrefix(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")

	res, err := h.objects.DeletePrefix(r.Context(), prefix, nil)
	if err != nil {
		h.writeObjectError(w, r, err, "failed to delete prefix", prefix)
		return
	}

	body := adminapi.ObjectDeleteResponse{Deleted: res.Deleted, Failed: res.Failed, Total: res.Total}
	if res.Failed > 0 {
		httputil.WriteJSON(w, http.StatusInternalServerError, body)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, body)
}

// writeObjectError renders a failed object operation. A rejection names what
// was wrong with the request; anything else is a server fault that logs the
// underlying error and reports the operator-facing message.
func (h *Handler) writeObjectError(w http.ResponseWriter, r *http.Request, err error, msg, key string) {
	switch {
	case errors.Is(err, ops.ErrKeyRequired),
		errors.Is(err, ops.ErrPrefixRequired),
		errors.Is(err, ops.ErrInvalidKey):
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ops.ErrNotFound):
		httputil.WriteJSONError(w, http.StatusNotFound, "not found")
	default:
		h.internalError(r.Context(), w, msg, err, slog.String("key", key))
	}
}

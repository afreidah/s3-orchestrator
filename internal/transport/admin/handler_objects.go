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
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
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
	prefix := q.Get(adminapi.ParamPrefix)

	delimiter := q.Get(adminapi.ParamDelimiter)
	if !q.Has(adminapi.ParamDelimiter) {
		delimiter = "/"
	}

	page, err := h.d.Ops.Objects.List(r.Context(), prefix, delimiter, q.Get(adminapi.ParamContinuation))
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
	ServeObject(w, r, h.log, h.d.Ops.Objects, r.PathValue(adminapi.ParamKey))
}

// handlePutObject stores the request body as one object. The body is the
// object bytes, so a caller uploads a file by streaming it directly rather
// than wrapping it in a form.
func (h *Handler) handlePutObject(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength < 0 {
		httputil.WriteJSONError(w, http.StatusLengthRequired, "content-length is required")
		return
	}
	if r.ContentLength > ops.MaxUploadSize {
		httputil.WriteJSONError(w, http.StatusRequestEntityTooLarge, "object exceeds the maximum upload size")
		return
	}

	body := http.MaxBytesReader(w, r.Body, ops.MaxUploadSize)
	defer body.Close()
	StoreObject(w, r, h.log, h.d.Ops.Objects, &object.PutObjectRequest{
		Key: r.PathValue(adminapi.ParamKey), Body: body, Size: r.ContentLength, ContentType: r.Header.Get(httputil.HeaderContentType),
	})
}

// handleDeleteObject removes one object and every copy of it.
func (h *Handler) handleDeleteObject(w http.ResponseWriter, r *http.Request) {
	DeleteObject(w, r, h.log, h.d.Ops.Objects, r.PathValue(adminapi.ParamKey))
}

// handleDeletePrefix removes every object under the given prefix.
func (h *Handler) handleDeletePrefix(w http.ResponseWriter, r *http.Request) {
	DeletePrefix(w, r, h.log, h.d.Ops.Objects, r.URL.Query().Get(adminapi.ParamPrefix))
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ServeObject streams key as a download, answering a rejected or missing key
// as the operation reports it. An interrupted download is logged.
func ServeObject(w http.ResponseWriter, r *http.Request, log *slog.Logger, objects *ops.Objects, key string) {
	result, err := objects.Get(r.Context(), key)
	if err != nil {
		httputil.WriteOpError(w, r, log, err, "failed to read object", "key", key)
		return
	}
	defer result.Body.Close()

	if err := httputil.ServeAttachment(w, key, result.Size, result.Body); err != nil {
		log.WarnContext(r.Context(), "object download interrupted", "key", key, "error", err)
	}
}

// StoreObject stores the request's body under its key and answers with the
// ETag it was recorded under.
func StoreObject(w http.ResponseWriter, r *http.Request, log *slog.Logger, objects *ops.Objects, req *object.PutObjectRequest) {
	etag, err := objects.Put(r.Context(), req)
	if err != nil {
		httputil.WriteOpError(w, r, log, err, "failed to store object", "key", req.Key)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectUploadResponse{ETag: etag})
}

// DeleteObject removes key and every copy of it, answering with the count.
func DeleteObject(w http.ResponseWriter, r *http.Request, log *slog.Logger, objects *ops.Objects, key string) {
	if err := objects.Delete(r.Context(), key); err != nil {
		httputil.WriteOpError(w, r, log, err, "failed to delete object", "key", key)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.ObjectDeleteResponse{Deleted: 1})
}

// DeletePrefix removes every object under prefix. The response carries the
// count, so a caller can tell a no-op from a mass removal, and a partial
// removal is a 500 carrying what it did remove.
func DeletePrefix(w http.ResponseWriter, r *http.Request, log *slog.Logger, objects *ops.Objects, prefix string) {
	res, err := objects.DeletePrefix(r.Context(), prefix)
	if err != nil {
		httputil.WriteOpError(w, r, log, err, "failed to delete prefix", "prefix", prefix)
		return
	}

	status := http.StatusOK
	if res.Failed > 0 {
		status = http.StatusInternalServerError
	}
	httputil.WriteJSON(w, status, adminapi.ObjectDeleteResponse{Deleted: res.Succeeded, Failed: res.Failed, Total: res.Attempted})
}

// -------------------------------------------------------------------------------
// UI Handler - Object Tree, Upload, Download, Delete
//
// Author: Alex Freidah
//
// JSON handlers for direct object-management actions: lazy-loaded directory
// tree, per-key delete and per-prefix bulk delete, multipart upload, and
// streaming download. Each parses the dashboard's request shape and calls the
// matching object operation, so the browser and a terminal client drive the
// same code.
// -------------------------------------------------------------------------------

package ui

import (
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// BROWSING
// -------------------------------------------------------------------------

// handleTreeAPI returns children of a directory prefix as JSON for the
// lazy-loaded file browser. The root lists only the buckets the user may list;
// below it, the prefix's bucket must grant list.
func (h *Handler) handleTreeAPI(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	if prefix != "" && !h.d.Buckets.HasPrefix(prefix) {
		httputil.WriteJSONError(w, http.StatusBadRequest, "prefix must start with a configured bucket name")
		return
	}
	if prefix != "" && !h.authorizeKey(w, r, prefix, core.PermList) {
		return
	}
	startAfter := r.URL.Query().Get("startAfter")
	maxKeys := httputil.QueryLimit(r.URL.Query().Get("maxKeys"), 200, 200)

	result, err := h.d.Dashboard.GetDirectoryChildren(r.Context(), prefix, startAfter, maxKeys)
	if err != nil {
		httputil.InternalError(r.Context(), h.log, w, "failed to list children", err, "prefix", prefix)
		return
	}
	if prefix == "" {
		result.Entries = listable(r.Context(), result.Entries)
	}

	httputil.WriteJSON(w, http.StatusOK, result)
}

// -------------------------------------------------------------------------
// DELETION
// -------------------------------------------------------------------------

// handleAPIDelete deletes a single object by key.
func (h *Handler) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	var req struct {
		Key string `json:"key"`
	}
	if !httputil.DecodeJSONBody(w, r, &req, httputil.MaxJSONBody) {
		return
	}
	if !h.authorizeKey(w, r, req.Key, core.PermDelete) {
		return
	}
	defer observeDuration("DELETE", time.Now())

	if err := h.d.Objects.Delete(r.Context(), req.Key); err != nil {
		httputil.WriteObjectError(w, r, h.log, err, "delete failed", req.Key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAPIDeletePrefix deletes all objects under a given key prefix.
func (h *Handler) handleAPIDeletePrefix(w http.ResponseWriter, r *http.Request) {
	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	var req struct {
		Prefix string `json:"prefix"`
	}
	if !httputil.DecodeJSONBody(w, r, &req, httputil.MaxJSONBody) {
		return
	}
	if !h.authorizeKey(w, r, req.Prefix, core.PermDelete) {
		return
	}
	defer observeDuration("DELETE", time.Now())

	res, err := h.d.Objects.DeletePrefix(r.Context(), req.Prefix, nil)
	if err != nil {
		httputil.WriteObjectError(w, r, h.log, err, "failed to list objects", req.Prefix)
		return
	}

	if res.Failed > 0 {
		httputil.WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   fmt.Sprintf("%d of %d deletes failed", res.Failed, res.Total),
			"deleted": res.Deleted,
		})
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": res.Deleted})
}

// -------------------------------------------------------------------------
// TRANSFER
// -------------------------------------------------------------------------

// handleAPIUpload uploads a file via multipart form data.
func (h *Handler) handleAPIUpload(w http.ResponseWriter, r *http.Request) {
	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, ops.MaxUploadSize)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "failed to parse form")
		return
	}

	key := r.FormValue("key")
	if !h.authorizeKey(w, r, key, core.PermWrite) {
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "file is required")
		return
	}
	defer file.Close()
	defer observeDuration("PUT", time.Now())

	etag, err := h.d.Objects.Put(r.Context(), key, file, header.Size, uploadContentType(header))
	if err != nil {
		httputil.WriteObjectError(w, r, h.log, err, "upload failed", key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "etag": etag})
}

// uploadContentType resolves the type to store an upload under, falling back
// to the filename extension when the browser sent nothing useful.
func uploadContentType(header *multipart.FileHeader) string {
	contentType := header.Header.Get(headerContentType)
	if contentType == "" || contentType == httputil.MediaOctetStream {
		if ct := mime.TypeByExtension(filepath.Ext(header.Filename)); ct != "" {
			return ct
		}
	}
	return contentType
}

// handleAPIDownload streams an object to the browser as a file download.
func (h *Handler) handleAPIDownload(w http.ResponseWriter, r *http.Request) {
	if !httputil.RequireMethod(w, r, http.MethodGet) {
		return
	}

	key := r.URL.Query().Get("key")
	if !h.authorizeKey(w, r, key, core.PermRead) {
		return
	}

	result, err := h.d.Objects.Get(r.Context(), key)
	if err != nil {
		httputil.WriteObjectError(w, r, h.log, err, "download failed", key)
		return
	}
	defer result.Body.Close()

	_ = httputil.ServeAttachment(w, key, result.Size, result.Body)
}

// observeDuration records how long an object operation has taken since start.
func observeDuration(method string, start time.Time) {
	telemetry.RequestDuration.WithLabelValues(method).Observe(time.Since(start).Seconds())
}

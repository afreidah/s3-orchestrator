// -------------------------------------------------------------------------------
// UI Handler - Object Tree, Upload, Download, Delete
//
// Author: Alex Freidah
//
// JSON handlers for direct object-management actions: lazy-loaded directory
// tree, per-key delete and per-prefix bulk delete, multipart upload, and
// streaming download. Each parses the dashboard's request shape, authorizes the
// key, and hands off to the admin API's own object functions, so the browser
// and a terminal client get the same responses.
// -------------------------------------------------------------------------------

package ui

import (
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
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
	maxKeys, ok := httputil.RequireQueryCount(w, r, "maxKeys", 200, 200)
	if !ok {
		return
	}

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
	admin.DeleteObject(w, r, h.log, h.d.Ops.Objects, req.Key)
}

// handleAPIDeletePrefix deletes all objects under a given key prefix.
func (h *Handler) handleAPIDeletePrefix(w http.ResponseWriter, r *http.Request) {
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
	admin.DeletePrefix(w, r, h.log, h.d.Ops.Objects, req.Prefix)
}

// -------------------------------------------------------------------------
// TRANSFER
// -------------------------------------------------------------------------

// handleAPIUpload uploads a file via multipart form data.
func (h *Handler) handleAPIUpload(w http.ResponseWriter, r *http.Request) {
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
	admin.StoreObject(w, r, h.log, h.d.Ops.Objects, &object.PutObjectRequest{
		Key: key, Body: file, Size: header.Size, ContentType: uploadContentType(header),
	})
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
	key := r.URL.Query().Get("key")
	if !h.authorizeKey(w, r, key, core.PermRead) {
		return
	}
	admin.ServeObject(w, r, h.log, h.d.Ops.Objects, key)
}

// observeDuration records how long an object operation has taken since start.
func observeDuration(method string, start time.Time) {
	telemetry.RequestDuration.WithLabelValues(method).Observe(time.Since(start).Seconds())
}

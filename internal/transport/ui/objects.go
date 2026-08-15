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
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
)

// handleTreeAPI returns children of a directory prefix as JSON for the
// lazy-loaded file browser.
func (h *Handler) handleTreeAPI(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	prefix := r.URL.Query().Get("prefix")
	if prefix != "" && !h.validBucketPrefix(prefix) {
		httputil.WriteJSONError(w, http.StatusBadRequest, "prefix must start with a configured bucket name")
		return
	}
	startAfter := r.URL.Query().Get("startAfter")
	maxKeys := 200
	if mk := r.URL.Query().Get("maxKeys"); mk != "" {
		if parsed, err := strconv.Atoi(mk); err == nil && parsed > 0 && parsed <= 200 {
			maxKeys = parsed
		}
	}

	result, err := h.dashboardOps.GetDirectoryChildren(r.Context(), prefix, startAfter, maxKeys)
	if err != nil {
		h.log.ErrorContext(r.Context(), "failed to list directory children", "prefix", prefix, "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "failed to list children")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, result)
}

// handleAPIDelete deletes a single object by key.
func (h *Handler) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	var req struct {
		Key string `json:"key"`
	}
	if !httputil.DecodeJSONBody(w, r, &req, 1<<20) {
		return
	}

	opStart := time.Now()
	defer func() {
		telemetry.RequestDuration.WithLabelValues("DELETE").Observe(time.Since(opStart).Seconds())
	}()

	if err := h.objects.Delete(r.Context(), req.Key); err != nil {
		h.writeObjectError(w, r, err, "delete failed", req.Key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAPIDeletePrefix deletes all objects under a given key prefix.
func (h *Handler) handleAPIDeletePrefix(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	var req struct {
		Prefix string `json:"prefix"`
	}
	if !httputil.DecodeJSONBody(w, r, &req, 1<<20) {
		return
	}

	opStart := time.Now()
	defer func() {
		telemetry.RequestDuration.WithLabelValues("DELETE").Observe(time.Since(opStart).Seconds())
	}()

	res, err := h.objects.DeletePrefix(r.Context(), req.Prefix, nil)
	if err != nil {
		h.writeObjectError(w, r, err, "failed to list objects", req.Prefix)
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

// handleAPIUpload uploads a file via multipart form data.
func (h *Handler) handleAPIUpload(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, ops.MaxUploadSize)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "failed to parse form")
		return
	}

	key := r.FormValue("key")
	file, header, err := r.FormFile("file")
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "file is required")
		return
	}
	defer file.Close()

	opStart := time.Now()
	defer func() {
		telemetry.RequestDuration.WithLabelValues("PUT").Observe(time.Since(opStart).Seconds())
	}()

	etag, err := h.objects.Put(r.Context(), key, file, header.Size, uploadContentType(header))
	if err != nil {
		h.writeObjectError(w, r, err, "upload failed", key)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "etag": etag})
}

// uploadContentType resolves the type to store an upload under, falling back
// to the filename extension when the browser sent nothing useful.
func uploadContentType(header *multipart.FileHeader) string {
	contentType := header.Header.Get(headerContentType)
	if contentType == "" || contentType == "application/octet-stream" {
		if ct := mime.TypeByExtension(filepath.Ext(header.Filename)); ct != "" {
			return ct
		}
	}
	return contentType
}

// handleAPIDownload streams an object to the browser as a file download.
func (h *Handler) handleAPIDownload(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if !httputil.RequireMethod(w, r, http.MethodGet) {
		return
	}

	key := r.URL.Query().Get("key")

	result, err := h.objects.Get(r.Context(), key)
	if err != nil {
		h.writeObjectError(w, r, err, "download failed", key)
		return
	}
	defer result.Body.Close()

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(key)))
	w.Header().Set(headerContentType, "application/octet-stream")
	if result.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	}

	_, _ = bufpool.Copy(w, result.Body)
}

// writeObjectError renders a failed object operation, keeping the rejection
// reasons the dashboard already understands distinct from a server fault.
func (h *Handler) writeObjectError(w http.ResponseWriter, r *http.Request, err error, msg, key string) {
	switch {
	case errors.Is(err, ops.ErrKeyRequired):
		httputil.WriteJSONError(w, http.StatusBadRequest, errKeyRequired)
	case errors.Is(err, ops.ErrPrefixRequired):
		httputil.WriteJSONError(w, http.StatusBadRequest, "prefix is required")
	case errors.Is(err, ops.ErrInvalidKey):
		httputil.WriteJSONError(w, http.StatusBadRequest, "key must start with a configured bucket name")
	case errors.Is(err, ops.ErrNotFound):
		httputil.WriteJSONError(w, http.StatusNotFound, "not found")
	default:
		h.log.ErrorContext(r.Context(), msg, "key", key, "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, msg)
	}
}

// -------------------------------------------------------------------------------
// UI Handler - Object Tree, Upload, Download, Delete
//
// Author: Alex Freidah
//
// JSON handlers for direct object-management actions: lazy-loaded
// directory tree, per-key delete and per-prefix bulk delete, multipart
// upload, and streaming download. Every handler that touches a real
// backend goes through ObjectManager so usage accounting fires through
// the same paths the S3-protocol handlers use.
// -------------------------------------------------------------------------------

package ui

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
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
	if req.Key == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, errKeyRequired)
		return
	}

	opStart := time.Now()
	defer func() {
		telemetry.RequestDuration.WithLabelValues("DELETE").Observe(time.Since(opStart).Seconds())
	}()

	if err := h.objects.DeleteObject(r.Context(), req.Key); err != nil {
		h.log.ErrorContext(r.Context(), "failed to delete object", "key", req.Key, "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "delete failed")
		return
	}

	h.log.InfoContext(r.Context(), "deleted object", "key", req.Key)
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
	if req.Prefix == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "prefix is required")
		return
	}

	opStart := time.Now()
	defer func() {
		telemetry.RequestDuration.WithLabelValues("DELETE").Observe(time.Since(opStart).Seconds())
	}()

	// Collect all object keys under the prefix via pagination.
	var keys []string
	startAfter := ""
	for {
		result, err := h.objects.ListObjects(r.Context(), req.Prefix, "", startAfter, 1000)
		if err != nil {
			h.log.ErrorContext(r.Context(), "failed to list objects for prefix delete", "prefix", req.Prefix, "error", err)
			httputil.WriteJSONError(w, http.StatusInternalServerError, "failed to list objects")
			return
		}
		for i := range result.Objects {
			keys = append(keys, result.Objects[i].ObjectKey)
		}
		if !result.IsTruncated {
			break
		}
		startAfter = result.NextContinuationToken
	}

	if len(keys) == 0 {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": 0})
		return
	}

	results := h.objects.DeleteObjects(r.Context(), keys)
	var errCount int
	for _, res := range results {
		if res.Err != nil {
			errCount++
		}
	}

	deleted := len(keys) - errCount
	h.log.InfoContext(r.Context(), "prefix delete completed", "prefix", req.Prefix, "deleted", deleted, "errors", errCount)

	if errCount > 0 {
		httputil.WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   fmt.Sprintf("%d of %d deletes failed", errCount, len(keys)),
			"deleted": deleted,
		})
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted})
}

// handleAPIUpload uploads a file via multipart form data.
func (h *Handler) handleAPIUpload(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if !httputil.RequireMethod(w, r, http.MethodPost) {
		return
	}

	const maxUploadSize = 512 << 20 // 512 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "failed to parse form")
		return
	}

	key := r.FormValue("key")
	if key == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, errKeyRequired)
		return
	}

	if !h.validBucketPrefix(key) {
		httputil.WriteJSONError(w, http.StatusBadRequest, "key must start with a configured bucket name")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "file is required")
		return
	}
	defer file.Close()

	contentType := header.Header.Get(headerContentType)
	if contentType == "" || contentType == "application/octet-stream" {
		if ct := mime.TypeByExtension(filepath.Ext(header.Filename)); ct != "" {
			contentType = ct
		}
	}

	opStart := time.Now()
	defer func() {
		telemetry.RequestDuration.WithLabelValues("PUT").Observe(time.Since(opStart).Seconds())
	}()

	etag, err := h.objects.PutObject(r.Context(), key, file, header.Size, contentType, nil)
	if err != nil {
		h.log.ErrorContext(r.Context(), "failed to upload object", "key", key, "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "upload failed")
		return
	}

	h.log.InfoContext(r.Context(), "uploaded object", "key", key, "size", header.Size)
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "etag": etag})
}

// handleAPIDownload streams an object to the browser as a file download.
func (h *Handler) handleAPIDownload(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if !httputil.RequireMethod(w, r, http.MethodGet) {
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, errKeyRequired)
		return
	}

	if !h.validBucketPrefix(key) {
		httputil.WriteJSONError(w, http.StatusBadRequest, "key must start with a configured bucket name")
		return
	}

	result, err := h.objects.GetObject(r.Context(), key, "")
	if err != nil {
		if errors.Is(err, core.ErrObjectNotFound) {
			httputil.WriteJSONError(w, http.StatusNotFound, "not found")
			return
		}
		h.log.ErrorContext(r.Context(), "failed to download object", "key", key, "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "download failed")
		return
	}
	defer result.Body.Close()

	h.log.InfoContext(r.Context(), "downloaded object", "key", key, "size", result.Size)

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(key)))
	w.Header().Set(headerContentType, "application/octet-stream")
	if result.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	}

	_, _ = bufpool.Copy(w, result.Body)
}

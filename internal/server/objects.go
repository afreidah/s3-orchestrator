// -------------------------------------------------------------------------------
// Object Handlers - PUT, GET, HEAD, DELETE, COPY, DeleteObjects
//
// Author: Alex Freidah
//
// HTTP handlers for standard S3 object operations. Streams response bodies
// directly to avoid buffering large objects in memory. DeleteObjects handles
// batch deletion of up to 1000 keys per request with concurrent backend I/O.
// -------------------------------------------------------------------------------

package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
)

// -------------------------------------------------------------------------
// XML TYPES
// -------------------------------------------------------------------------

// copyObjectResult is the XML response for CopyObject.
type copyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	Xmlns        string   `xml:"xmlns,attr"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

// deleteObjectsRequest is the XML request body for DeleteObjects.
type deleteObjectsRequest struct {
	Quiet   bool                `xml:"Quiet"`
	Objects []deleteObjectEntry `xml:"Object"`
}

type deleteObjectEntry struct {
	Key string `xml:"Key"`
}

// deleteObjectsResult is the XML response for DeleteObjects.
type deleteObjectsResult struct {
	XMLName xml.Name            `xml:"DeleteResult"`
	Xmlns   string              `xml:"xmlns,attr"`
	Deleted []deletedObject     `xml:"Deleted,omitempty"`
	Errors  []deleteObjectError `xml:"Error,omitempty"`
}

type deletedObject struct {
	Key string `xml:"Key"`
}

type deleteObjectError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// -------------------------------------------------------------------------
// OBJECT HANDLERS
// -------------------------------------------------------------------------

// handlePut processes PUT requests. Requires Content-Length header to enforce
// size-based policies in later phases.
func (s *Server) handlePut(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, error) {
	if r.ContentLength < 0 {
		writeS3Error(w, http.StatusLengthRequired, "MissingContentLength", "Content-Length header is required")
		return http.StatusLengthRequired, fmt.Errorf("missing Content-Length")
	}

	if s.MaxObjectSize > 0 && r.ContentLength > s.MaxObjectSize {
		writeS3Error(w, http.StatusRequestEntityTooLarge, "EntityTooLarge", "Object size exceeds the maximum allowed size")
		return http.StatusRequestEntityTooLarge, fmt.Errorf("object size %d exceeds max %d", r.ContentLength, s.MaxObjectSize)
	}

	if s.MaxObjectSize > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, s.MaxObjectSize)
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// --- Add size to span ---
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		telemetry.AttrObjectSize.Int64(r.ContentLength),
		telemetry.AttrContentType.String(contentType),
	)

	etag, err := s.Manager.PutObject(ctx, key, r.Body, r.ContentLength, contentType)
	if err != nil {
		return writeStorageError(w, err, "Failed to store object"), err
	}

	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(http.StatusOK)
	return http.StatusOK, nil
}

// handleGet processes GET requests. Streams the response body directly to avoid
// buffering large objects in memory. Supports Range requests — when the client
// sends a Range header, the response is 206 Partial Content with Content-Range.
func (s *Server) handleGet(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, int64, error) {
	rangeHeader := r.Header.Get("Range")

	result, err := s.Manager.GetObject(ctx, key, rangeHeader)
	if err != nil {
		return writeStorageError(w, err, "Failed to retrieve object"), 0, err
	}
	defer func() { _ = result.Body.Close() }()

	// --- Add size to span ---
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		telemetry.AttrObjectSize.Int64(result.Size),
		telemetry.AttrContentType.String(result.ContentType),
	)

	w.Header().Set("Content-Type", result.ContentType)
	if result.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	}
	if result.ETag != "" {
		w.Header().Set("ETag", result.ETag)
	}
	w.Header().Set("Accept-Ranges", "bytes")

	status := http.StatusOK
	if result.ContentRange != "" {
		w.Header().Set("Content-Range", result.ContentRange)
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)

	written, copyErr := io.Copy(w, result.Body)
	if copyErr != nil {
		return status, written, fmt.Errorf("error streaming body: %w", copyErr)
	}

	return status, written, nil
}

// handleHead processes HEAD requests.
func (s *Server) handleHead(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, error) {
	size, contentType, etag, err := s.Manager.HeadObject(ctx, key)
	if err != nil {
		return writeStorageError(w, err, "Failed to retrieve object metadata"), err
	}

	// --- Add size to span ---
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		telemetry.AttrObjectSize.Int64(size),
		telemetry.AttrContentType.String(contentType),
	)

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)
	return http.StatusOK, nil
}

// handleDelete processes DELETE requests. The manager treats missing objects as
// success (S3 idempotent delete), so any error returned is a real backend failure.
func (s *Server) handleDelete(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, error) {
	if err := s.Manager.DeleteObject(ctx, key); err != nil {
		return writeStorageError(w, err, "Failed to delete object"), err
	}

	w.WriteHeader(http.StatusNoContent)
	return http.StatusNoContent, nil
}

// handleCopyObject processes PUT requests with the x-amz-copy-source header.
// Copies an object from the source key to the destination key, potentially
// across backends, with atomic quota tracking. Only same-bucket copies are
// allowed (no cross-bucket copying).
func (s *Server) handleCopyObject(ctx context.Context, w http.ResponseWriter, r *http.Request, bucket, destInternalKey, copySource string) (int, error) {
	// --- Parse x-amz-copy-source header (may be URL-encoded) ---
	decoded, err := url.PathUnescape(copySource)
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "Invalid x-amz-copy-source encoding")
		return http.StatusBadRequest, fmt.Errorf("invalid copy source encoding: %w", err)
	}
	source := strings.TrimPrefix(decoded, "/")
	sourceBucket, sourceKey, ok := parsePath("/" + source)
	if !ok || sourceKey == "" {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "Invalid x-amz-copy-source")
		return http.StatusBadRequest, fmt.Errorf("invalid copy source: %s", copySource)
	}

	// --- Validate source bucket matches authorized bucket (same-bucket only) ---
	if sourceBucket != bucket {
		writeS3Error(w, http.StatusForbidden, "AccessDenied",
			fmt.Sprintf("Cross-bucket copy not allowed: source bucket %s does not match %s", sourceBucket, bucket))
		return http.StatusForbidden, fmt.Errorf("cross-bucket copy denied: %s != %s", sourceBucket, bucket)
	}

	// Prefix source key for internal storage
	sourceInternalKey := bucket + "/" + sourceKey

	etag, err := s.Manager.CopyObject(ctx, sourceInternalKey, destInternalKey)
	if err != nil {
		return writeStorageError(w, err, "Failed to copy object"), err
	}

	result := copyObjectResult{
		Xmlns:        "http://s3.amazonaws.com/doc/2006-03-01/",
		ETag:         etag,
		LastModified: time.Now().UTC().Format(time.RFC3339),
	}

	if err := writeXML(w, http.StatusOK, result); err != nil {
		return http.StatusOK, fmt.Errorf("failed to encode copy response: %w", err)
	}
	return http.StatusOK, nil
}

// handleDeleteObjects processes POST /{bucket}?delete requests. Deletes up to
// 1000 objects per request and returns per-key results in an XML response.
// Per the S3 spec, HTTP 200 is always returned even when individual keys fail.
func (s *Server) handleDeleteObjects(ctx context.Context, w http.ResponseWriter, r *http.Request, bucket string) (int, error) {
	// Parse XML request body
	var req deleteObjectsRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "Failed to parse request body")
		return http.StatusBadRequest, fmt.Errorf("failed to decode delete request: %w", err)
	}

	if len(req.Objects) == 0 {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "No objects specified for deletion")
		return http.StatusBadRequest, fmt.Errorf("empty delete request")
	}
	if len(req.Objects) > 1000 {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML",
			"DeleteObjects request may contain a maximum of 1000 objects")
		return http.StatusBadRequest, fmt.Errorf("too many objects: %d", len(req.Objects))
	}

	// Prefix each key with the bucket for internal storage
	keys := make([]string, len(req.Objects))
	for i, obj := range req.Objects {
		keys[i] = bucket + "/" + obj.Key
	}

	results := s.Manager.DeleteObjects(ctx, keys)

	// Build XML response with per-key outcomes
	resp := deleteObjectsResult{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}
	for i, res := range results {
		userKey := req.Objects[i].Key
		if res.Err != nil {
			resp.Errors = append(resp.Errors, deleteObjectError{
				Key:     userKey,
				Code:    "InternalError",
				Message: "Failed to delete object",
			})
		} else if !req.Quiet {
			resp.Deleted = append(resp.Deleted, deletedObject{Key: userKey})
		}
	}

	if err := writeXML(w, http.StatusOK, resp); err != nil {
		return http.StatusOK, fmt.Errorf("failed to encode delete objects response: %w", err)
	}
	return http.StatusOK, nil
}

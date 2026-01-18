// -------------------------------------------------------------------------------
// HTTP Handlers - S3 Request Processing
//
// Project: Munchbox / Author: Alex Freidah
//
// HTTP server and request handlers for S3-compatible operations. Implements a
// subset of the S3 API sufficient for basic object storage: PUT, GET, HEAD,
// DELETE. Returns S3-style XML error responses for compatibility with S3 clients.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// -------------------------------------------------------------------------
// SERVER
// -------------------------------------------------------------------------

// Server handles HTTP requests and routes them to the backend manager.
type Server struct {
	Manager       *BackendManager
	VirtualBucket string
	AuthToken     string
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	method := r.Method

	// --- Track inflight requests ---
	InflightRequests.WithLabelValues(method).Inc()
	defer InflightRequests.WithLabelValues(method).Dec()

	// --- Auth check ---
	if s.AuthToken != "" {
		if r.Header.Get("X-Proxy-Token") != s.AuthToken {
			s.recordRequest(method, http.StatusForbidden, start, 0, 0)
			log.Printf("[%s] %s %s - 403 Forbidden (auth failed)", method, r.URL.Path, r.RemoteAddr)
			writeS3Error(w, http.StatusForbidden, "AccessDenied", "Invalid or missing authentication token")
			return
		}
	}

	// --- Parse path ---
	bucket, key, ok := parsePath(r.URL.Path)
	if !ok {
		s.recordRequest(method, http.StatusBadRequest, start, 0, 0)
		log.Printf("[%s] %s %s - 400 Bad Request (invalid path)", method, r.URL.Path, r.RemoteAddr)
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest", "Invalid path format")
		return
	}

	// --- Verify bucket name ---
	if bucket != s.VirtualBucket {
		s.recordRequest(method, http.StatusNotFound, start, 0, 0)
		log.Printf("[%s] %s %s - 404 Not Found (unknown bucket: %s)", method, r.URL.Path, r.RemoteAddr, bucket)
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", fmt.Sprintf("Bucket %s not found", bucket))
		return
	}

	// --- Start tracing span ---
	ctx, span := StartSpan(r.Context(), fmt.Sprintf("HTTP %s", method),
		RequestAttributes(method, r.URL.Path, bucket, key, r.RemoteAddr)...,
	)
	defer span.End()

	// --- Create context with timeout ---
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// --- Route by method ---
	var status int
	var err error
	var requestSize, responseSize int64

	switch method {
	case http.MethodPut:
		requestSize = r.ContentLength
		status, err = s.handlePut(ctx, w, r, key)
	case http.MethodGet:
		status, responseSize, err = s.handleGet(ctx, w, r, key)
	case http.MethodHead:
		status, err = s.handleHead(ctx, w, r, key)
	case http.MethodDelete:
		status, err = s.handleDelete(ctx, w, r, key)
	default:
		s.recordRequest(method, http.StatusMethodNotAllowed, start, 0, 0)
		log.Printf("[%s] %s %s - 405 Method Not Allowed", method, r.URL.Path, r.RemoteAddr)
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not supported")
		span.SetStatus(codes.Error, "method not allowed")
		return
	}

	// --- Record metrics ---
	s.recordRequest(method, status, start, requestSize, responseSize)

	// --- Update span status ---
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
	}
	span.SetAttributes(attribute.Int("http.status_code", status))

	// --- Log request ---
	elapsed := time.Since(start)
	if err != nil {
		log.Printf("[%s] %s %s - %d (%v) [%v]", method, r.URL.Path, r.RemoteAddr, status, err, elapsed)
	} else {
		log.Printf("[%s] %s %s - %d [%v]", method, r.URL.Path, r.RemoteAddr, status, elapsed)
	}
}

// recordRequest updates Prometheus metrics for a completed request.
func (s *Server) recordRequest(method string, status int, start time.Time, reqSize, respSize int64) {
	statusStr := strconv.Itoa(status)
	RequestsTotal.WithLabelValues(method, statusStr).Inc()
	RequestDuration.WithLabelValues(method).Observe(time.Since(start).Seconds())

	if reqSize > 0 {
		RequestSize.WithLabelValues(method).Observe(float64(reqSize))
	}
	if respSize > 0 {
		ResponseSize.WithLabelValues(method).Observe(float64(respSize))
	}
}

// -------------------------------------------------------------------------
// HANDLERS
// -------------------------------------------------------------------------

// handlePut processes PUT requests. Requires Content-Length header to enforce
// size-based policies in later phases.
func (s *Server) handlePut(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, error) {
	if r.ContentLength < 0 {
		writeS3Error(w, http.StatusLengthRequired, "MissingContentLength", "Content-Length header is required")
		return http.StatusLengthRequired, fmt.Errorf("missing Content-Length")
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// --- Add size to span ---
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		AttrObjectSize.Int64(r.ContentLength),
		AttrContentType.String(contentType),
	)

	etag, err := s.Manager.PutObject(ctx, key, r.Body, r.ContentLength, contentType)
	if err != nil {
		if errors.Is(err, ErrInsufficientStorage) {
			writeS3Error(w, http.StatusInsufficientStorage, "InsufficientStorage", "No backend has sufficient quota")
			return http.StatusInsufficientStorage, err
		}
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to store object")
		return http.StatusBadGateway, err
	}

	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(http.StatusOK)
	return http.StatusOK, nil
}

// handleGet processes GET requests. Streams the response body directly to avoid
// buffering large objects in memory.
func (s *Server) handleGet(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, int64, error) {
	body, size, contentType, etag, err := s.Manager.GetObject(ctx, key)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey", "Object not found")
			return http.StatusNotFound, 0, err
		}
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to retrieve object")
		return http.StatusBadGateway, 0, err
	}
	defer body.Close()

	// --- Add size to span ---
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		AttrObjectSize.Int64(size),
		AttrContentType.String(contentType),
	)

	w.Header().Set("Content-Type", contentType)
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}

	written, copyErr := io.Copy(w, body)
	if copyErr != nil {
		return http.StatusOK, written, fmt.Errorf("error streaming body: %w", copyErr)
	}

	return http.StatusOK, written, nil
}

// handleHead processes HEAD requests.
func (s *Server) handleHead(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, error) {
	size, contentType, etag, err := s.Manager.HeadObject(ctx, key)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey", "Object not found")
			return http.StatusNotFound, err
		}
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to retrieve object metadata")
		return http.StatusBadGateway, err
	}

	// --- Add size to span ---
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		AttrObjectSize.Int64(size),
		AttrContentType.String(contentType),
	)

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(http.StatusOK)
	return http.StatusOK, nil
}

// handleDelete processes DELETE requests. Treats missing objects as success
// since S3 DELETE is typically idempotent.
func (s *Server) handleDelete(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, error) {
	err := s.Manager.DeleteObject(ctx, key)
	if err != nil {
		log.Printf("Delete error (treating as success): %v", err)
	}

	w.WriteHeader(http.StatusNoContent)
	return http.StatusNoContent, nil
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// parsePath extracts bucket and key from the URL path.
// Expected format: /{bucket}/{key...}
func parsePath(path string) (bucket string, key string, ok bool) {
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// writeS3Error sends an S3-style XML error response.
func writeS3Error(w http.ResponseWriter, code int, errCode, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error>
  <Code>%s</Code>
  <Message>%s</Message>
</Error>`, xmlEscape(errCode), xmlEscape(message))
}

// xmlEscape escapes special XML characters.
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}

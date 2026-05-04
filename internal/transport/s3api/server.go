// -------------------------------------------------------------------------------
// HTTP Server - S3 Request Routing
//
// Author: Alex Freidah
//
// HTTP server and request router for S3-compatible operations. Implements a
// subset of the S3 API sufficient for basic object storage: PUT, GET, HEAD,
// DELETE. Routes requests to the appropriate handler based on method and query
// parameters. Supports multi-bucket isolation via credential-based bucket
// resolution and internal key prefixing.
// -------------------------------------------------------------------------------

// Package server implements the S3-compatible HTTP API, routing requests to the
// storage backend manager with authentication, rate limiting, and tracing.
package s3api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// -------------------------------------------------------------------------
// SERVER
// -------------------------------------------------------------------------

// httpSpanName maps HTTP methods to pre-computed span names, avoiding
// fmt.Sprintf allocation on every request.
var httpSpanName = map[string]string{
	http.MethodGet:    "HTTP GET",
	http.MethodPut:    "HTTP PUT",
	http.MethodHead:   "HTTP HEAD",
	http.MethodDelete: "HTTP DELETE",
	http.MethodPost:   "HTTP POST",
}

// Server handles HTTP requests and routes them to the backend manager.
type Server struct {
	Manager       *proxy.BackendManager
	bucketAuth    syncutil.AtomicConfig[auth.BucketRegistry]
	MaxObjectSize int64     // Max upload body size in bytes
	startedAt     time.Time // Stable timestamp for ListBuckets CreationDate
}

// NewServer creates a Server with a stable start timestamp.
func NewServer(manager *proxy.BackendManager, maxObjectSize int64) *Server {
	return &Server{
		Manager:       manager,
		MaxObjectSize: maxObjectSize,
		startedAt:     time.Now(),
	}
}

// SetBucketAuth atomically replaces the bucket authentication registry.
// Safe to call concurrently with request handling.
func (s *Server) SetBucketAuth(br *auth.BucketRegistry) {
	s.bucketAuth.Store(br)
}

// GetBucketAuth returns the current bucket authentication registry.
func (s *Server) GetBucketAuth() *auth.BucketRegistry {
	return s.bucketAuth.Load()
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	method := r.Method

	requestID := r.Header.Get("X-Request-Id")
	if !isValidRequestID(requestID) {
		requestID = audit.NewID()
	}
	ctx := audit.WithRequestID(r.Context(), requestID)
	w.Header().Set("X-Amz-Request-Id", requestID)

	telemetry.InflightRequests.WithLabelValues(method).Inc()
	defer telemetry.InflightRequests.WithLabelValues(method).Dec()

	authorizedBucket, err := s.GetBucketAuth().AuthenticateAndResolveBucket(r)
	if err != nil {
		s.rejectAuth(ctx, w, r, method, start, err)
		return
	}

	if r.URL.Path == "/" && method == http.MethodGet {
		s.serveListBuckets(ctx, w, r, method, requestID, start, authorizedBucket)
		return
	}

	bucket, key, ok := parsePath(r.URL.Path)
	if !ok {
		s.rejectInvalidPath(ctx, w, r, method, start)
		return
	}
	if bucket != authorizedBucket {
		s.rejectBucketMismatch(ctx, w, r, method, start, authorizedBucket, bucket)
		return
	}

	internalKey := internalkey.Make(bucket, key)

	spanName := httpSpanName[method]
	if spanName == "" {
		spanName = "HTTP " + method
	}
	ctx, span := telemetry.StartServerSpan(ctx, spanName,
		append(telemetry.RequestAttributes(method, r.URL.Path, bucket, key, r.RemoteAddr),
			telemetry.AttrRequestID.String(requestID))...,
	)
	defer span.End()

	rejectMethod := func(msg string) {
		s.recordRequest(method, http.StatusMethodNotAllowed, start, 0, 0)
		audit.Log(ctx, "s3.MethodNotAllowed",
			slog.String("method", method),
			slog.String("path", r.URL.Path),
			slog.String("remote", r.RemoteAddr),
			slog.String("bucket", bucket),
			slog.Int("status", http.StatusMethodNotAllowed),
			slog.Duration("duration", time.Since(start)),
		)
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", msg)
		span.SetStatus(codes.Error, msg)
	}

	var status int
	var requestSize, responseSize int64
	var operation string
	var supported bool

	if key == "" {
		operation, status, err, supported = s.routeBucketRequest(ctx, w, r, method, bucket)
	} else {
		operation, status, requestSize, responseSize, err, supported = s.routeObjectRequest(ctx, w, r, method, bucket, key, internalKey)
	}
	if !supported {
		if key == "" {
			rejectMethod("Method not supported for bucket")
		} else {
			rejectMethod("Method not supported")
		}
		return
	}

	s.recordRequest(method, status, start, requestSize, responseSize)

	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		slog.WarnContext(ctx, "S3 request failed", "operation", operation, "key", key, "status", status, "remote", r.RemoteAddr, "error", err)
	}
	span.SetAttributes(attribute.Int("http.status_code", status))

	s.auditRequest(ctx, r, method, bucket, key, operation, status, requestSize, responseSize, time.Since(start), err)
}

// rejectAuth writes a 403 AccessDenied response after authentication
// failure and emits the matching audit log.
func (s *Server) rejectAuth(ctx context.Context, w http.ResponseWriter, r *http.Request, method string, start time.Time, err error) {
	s.recordRequest(method, http.StatusForbidden, start, 0, 0)
	slog.WarnContext(ctx, "S3 auth failure", "method", method, "path", r.URL.Path, "remote", r.RemoteAddr, "error", err)
	audit.Log(ctx, "s3.AuthFailure",
		slog.String("method", method),
		slog.String("path", r.URL.Path),
		slog.String("remote", r.RemoteAddr),
		slog.String("error", err.Error()),
		slog.Int("status", http.StatusForbidden),
		slog.Duration("duration", time.Since(start)),
	)
	writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access denied")
}

// serveListBuckets handles the special-case GET / route that serves the
// authorized bucket as a single-entry ListBuckets response.
func (s *Server) serveListBuckets(ctx context.Context, w http.ResponseWriter, r *http.Request, method, requestID string, start time.Time, authorizedBucket string) {
	ctx, span := telemetry.StartServerSpan(ctx, "HTTP GET",
		append(telemetry.RequestAttributes(method, "/", "", "", r.RemoteAddr),
			telemetry.AttrRequestID.String(requestID))...,
	)
	defer span.End()

	status, err := s.handleListBuckets(w, authorizedBucket)
	s.recordRequest(method, status, start, 0, 0)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
	}
	span.SetAttributes(attribute.Int("http.status_code", status))
	audit.Log(ctx, "s3.ListBuckets",
		slog.String("operation", "ListBuckets"),
		slog.String("method", method),
		slog.String("path", r.URL.Path),
		slog.String("remote", r.RemoteAddr),
		slog.Int("status", status),
		slog.Duration("duration", time.Since(start)),
	)
}

// rejectInvalidPath writes a 400 InvalidRequest for a path that does not
// parse as bucket[/key].
func (s *Server) rejectInvalidPath(ctx context.Context, w http.ResponseWriter, r *http.Request, method string, start time.Time) {
	s.recordRequest(method, http.StatusBadRequest, start, 0, 0)
	audit.Log(ctx, "s3.InvalidPath",
		slog.String("method", method),
		slog.String("path", r.URL.Path),
		slog.String("remote", r.RemoteAddr),
		slog.Int("status", http.StatusBadRequest),
		slog.Duration("duration", time.Since(start)),
	)
	writeS3Error(w, http.StatusBadRequest, "InvalidRequest", "Invalid path format")
}

// rejectBucketMismatch writes a 403 AccessDenied when the path bucket
// does not match the bucket the credentials authorize.
func (s *Server) rejectBucketMismatch(ctx context.Context, w http.ResponseWriter, r *http.Request, method string, start time.Time, authorizedBucket, bucket string) {
	s.recordRequest(method, http.StatusForbidden, start, 0, 0)
	slog.WarnContext(ctx, "S3 bucket mismatch", "method", method, "path", r.URL.Path, "remote", r.RemoteAddr, "authorized", authorizedBucket, "requested", bucket)
	audit.Log(ctx, "s3.BucketMismatch",
		slog.String("method", method),
		slog.String("path", r.URL.Path),
		slog.String("remote", r.RemoteAddr),
		slog.String("authorized_bucket", authorizedBucket),
		slog.String("requested_bucket", bucket),
		slog.Int("status", http.StatusForbidden),
		slog.Duration("duration", time.Since(start)),
	)
	writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access denied")
}

// routeBucketRequest dispatches bucket-level operations (no object key)
// to their handlers. supported=false means none of the supported method/
// query combinations matched; the caller emits a 405.
func (s *Server) routeBucketRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, method, bucket string) (string, int, error, bool) {
	query := r.URL.Query()
	_, hasDelete := query["delete"]
	_, hasLocation := query["location"]
	_, hasUploads := query["uploads"]
	_, hasVersioning := query["versioning"]

	switch {
	case method == http.MethodHead:
		s, e := s.handleHeadBucket(w)
		return "HeadBucket", s, e, true
	case method == http.MethodGet && hasVersioning:
		st, e := s.handleGetBucketVersioning(w)
		return "GetBucketVersioning", st, e, true
	case method == http.MethodGet && hasUploads:
		st, e := s.handleListMultipartUploads(ctx, w, r, bucket)
		return "ListMultipartUploads", st, e, true
	case method == http.MethodGet && hasLocation:
		st, e := s.handleGetBucketLocation(w)
		return "GetBucketLocation", st, e, true
	case method == http.MethodGet && query.Get("list-type") == "2":
		st, e := s.handleListObjectsV2(ctx, w, r, bucket)
		return "ListObjectsV2", st, e, true
	case method == http.MethodGet:
		st, e := s.handleListObjectsV1(ctx, w, r, bucket)
		return "ListObjectsV1", st, e, true
	case method == http.MethodPost && hasDelete:
		st, e := s.handleDeleteObjects(ctx, w, r, bucket)
		return "DeleteObjects", st, e, true
	}
	return "", 0, nil, false
}

// routeObjectRequest dispatches object-level operations to their
// handlers, splitting on multipart-upload state. supported=false means
// the method/query combination is not supported and the caller emits 405.
func (s *Server) routeObjectRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, method, bucket, key, internalKey string) (string, int, int64, int64, error, bool) {
	query := r.URL.Query()
	_, hasUploads := query["uploads"]
	uploadID := query.Get("uploadId")

	switch {
	case hasUploads && method == http.MethodPost:
		st, e := s.handleCreateMultipartUpload(ctx, w, r, bucket, key, internalKey)
		return "CreateMultipartUpload", st, 0, 0, e, true
	case uploadID != "":
		return s.routeMultipartRequest(ctx, w, r, method, bucket, key, internalKey, uploadID)
	default:
		return s.routePlainObjectRequest(ctx, w, r, method, bucket, internalKey)
	}
}

// routeMultipartRequest dispatches per-uploadID multipart operations.
func (s *Server) routeMultipartRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, method, bucket, key, internalKey, uploadID string) (string, int, int64, int64, error, bool) {
	switch method {
	case http.MethodPut:
		st, e := s.handleUploadPart(ctx, w, r, internalKey)
		return "UploadPart", st, r.ContentLength, 0, e, true
	case http.MethodPost:
		st, e := s.handleCompleteMultipartUpload(ctx, w, r, bucket, key)
		return "CompleteMultipartUpload", st, 0, 0, e, true
	case http.MethodDelete:
		st, e := s.handleAbortMultipartUpload(ctx, w, uploadID)
		return "AbortMultipartUpload", st, 0, 0, e, true
	case http.MethodGet:
		st, e := s.handleListParts(ctx, w, r, bucket, key, internalKey)
		return "ListParts", st, 0, 0, e, true
	}
	return "", 0, 0, 0, nil, false
}

// routePlainObjectRequest dispatches non-multipart object operations.
// PUT splits between PutObject and CopyObject based on the
// X-Amz-Copy-Source header.
func (s *Server) routePlainObjectRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, method, bucket, internalKey string) (string, int, int64, int64, error, bool) {
	switch method {
	case http.MethodPut:
		if copySource := r.Header.Get("X-Amz-Copy-Source"); copySource != "" {
			st, e := s.handleCopyObject(ctx, w, r, bucket, internalKey, copySource)
			return "CopyObject", st, 0, 0, e, true
		}
		st, e := s.handlePut(ctx, w, r, internalKey)
		return "PutObject", st, r.ContentLength, 0, e, true
	case http.MethodGet:
		st, sz, e := s.handleGet(ctx, w, r, internalKey)
		return "GetObject", st, 0, sz, e, true
	case http.MethodHead:
		st, e := s.handleHead(ctx, w, r, internalKey)
		return "HeadObject", st, 0, 0, e, true
	case http.MethodDelete:
		st, e := s.handleDelete(ctx, w, r, internalKey)
		return "DeleteObject", st, 0, 0, e, true
	}
	return "", 0, 0, 0, nil, false
}

// auditRequest emits the per-request audit log entry. Path, method,
// bucket, status, and duration are always present; key, sizes, and error
// are appended only when they carry information.
func (s *Server) auditRequest(ctx context.Context, r *http.Request, method, bucket, key, operation string, status int, requestSize, responseSize int64, elapsed time.Duration, err error) {
	attrs := []slog.Attr{
		slog.String("operation", operation),
		slog.String("method", method),
		slog.String("path", r.URL.Path),
		slog.String("remote", r.RemoteAddr),
		slog.String("bucket", bucket),
		slog.Int("status", status),
		slog.Duration("duration", elapsed),
	}
	if key != "" {
		attrs = append(attrs, slog.String("key", key))
	}
	if requestSize > 0 {
		attrs = append(attrs, slog.Int64("request_size", requestSize))
	}
	if responseSize > 0 {
		attrs = append(attrs, slog.Int64("response_size", responseSize))
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	audit.Log(ctx, "s3."+operation, attrs...)
}

// -------------------------------------------------------------------------
// METRICS
// -------------------------------------------------------------------------

// isValidRequestID checks that a client-supplied request ID is safe to
// propagate into logs and response headers (hex chars only, max 64 chars).
func isValidRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// statusStrings maps common HTTP status codes to pre-computed strings,
// avoiding strconv.Itoa allocation per request.
var statusStrings = map[int]string{
	200: "200", 204: "204", 206: "206",
	304: "304",
	400: "400", 403: "403", 404: "404", 405: "405", 411: "411", 413: "413", 429: "429",
	500: "500", 502: "502", 503: "503", 507: "507",
}

// recordRequest updates Prometheus metrics for a completed request.
func (s *Server) recordRequest(method string, status int, start time.Time, reqSize, respSize int64) {
	statusStr, ok := statusStrings[status]
	if !ok {
		statusStr = strconv.Itoa(status)
	}
	telemetry.RequestsTotal.WithLabelValues(method, statusStr).Inc()
	telemetry.RequestDuration.WithLabelValues(method).Observe(time.Since(start).Seconds())

	if reqSize > 0 {
		telemetry.RequestSize.WithLabelValues(method).Observe(float64(reqSize))
	}
	if respSize > 0 {
		telemetry.ResponseSize.WithLabelValues(method).Observe(float64(respSize))
	}
}

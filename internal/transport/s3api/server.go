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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"

	"go.opentelemetry.io/otel/attribute"
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

//go:generate mockgen -destination=mock_ops_test.go -package=s3api github.com/afreidah/s3-orchestrator/internal/transport/s3api ObjectOps,MultipartOps

// ObjectOps is the narrow object surface the S3 transport depends on:
// the object CRUD, listing, and capacity queries reachable from an S3
// request. *object.Manager satisfies it.
type ObjectOps interface {
	PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error)
	GetObject(ctx context.Context, key, rangeHeader string) (*s3be.GetObjectResult, error)
	HeadObject(ctx context.Context, key string) (*s3be.HeadObjectResult, error)
	DeleteObject(ctx context.Context, key string) error
	DeleteObjects(ctx context.Context, keys []string) []object.DeleteObjectResult
	CopyObject(ctx context.Context, sourceKey, destKey string) (string, error)
	ListObjects(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*object.ListObjectsV2Result, error)
	ObjectExists(ctx context.Context, key string) (bool, error)
	CanAcceptWrite(size int64) bool
	BackendCapacityStats(ctx context.Context) map[string]core.QuotaStat
	GetObjectTags(ctx context.Context, key string) ([]core.Tag, error)
	PutObjectTags(ctx context.Context, key string, tags []core.Tag) error
	DeleteObjectTags(ctx context.Context, key string) error
}

// MultipartOps is the narrow multipart surface the S3 transport depends on.
// *multipart.Manager satisfies it.
type MultipartOps interface {
	CreateMultipartUpload(ctx context.Context, key, contentType string, metadata map[string]string) (string, string, error)
	UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, body io.Reader, size int64) (string, error)
	CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, manifest []core.CompletePart) (string, error)
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error
	ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]core.MultipartUpload, error)
	GetParts(ctx context.Context, bucket, key, uploadID string) ([]core.MultipartPart, error)
	CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error)
}

// Compile-time assertions.
var (
	_ ObjectOps    = (*object.Manager)(nil)
	_ MultipartOps = (*multipart.Manager)(nil)
)

// Server handles HTTP requests and routes them to the object and multipart
// managers.
type Server struct {
	Objects       ObjectOps
	Multipart     MultipartOps
	bucketAuth    syncutil.AtomicConfig[auth.BucketRegistry]
	MaxObjectSize int64        // Max upload body size in bytes
	startedAt     time.Time    // Stable timestamp for ListBuckets CreationDate
	log           *slog.Logger // scoped to logfmt.Component("s3_server")
}

// logger returns the component-scoped logger, falling back to
// slog.Default() when the server was constructed by a test that did
// not set the log field. The handler chain still applies (ErrAttrHandler
// + Component scoping comes from the runtime default).
func (s *Server) logger() *slog.Logger {
	if s.log == nil {
		return slog.Default()
	}
	return s.log
}

// NewServer creates a Server with a stable start timestamp.
func NewServer(objects ObjectOps, multipartMgr MultipartOps, maxObjectSize int64) *Server {
	must.NotNil("objects", objects)
	must.NotNil("multipart", multipartMgr)
	return &Server{
		Objects:       objects,
		Multipart:     multipartMgr,
		MaxObjectSize: maxObjectSize,
		startedAt:     time.Now(),
		log:           slog.Default().With(logfmt.Component("s3_server")),
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

	authorizedBucket, streamMat, err := s.GetBucketAuth().AuthenticateAndResolveBucket(r)
	if err != nil {
		s.rejectAuth(ctx, w, r, method, start, err)
		return
	}
	if streamMat != nil {
		applyStreamingBody(r, streamMat)
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
			slog.String("client_addr", r.RemoteAddr),
			slog.String("bucket", bucket),
			slog.Int("status", http.StatusMethodNotAllowed),
			slog.Duration("duration", time.Since(start)),
		)
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", msg)
		observe.MarkSpanError(span, msg)
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
		observe.RecordSpanError(span, err)
		slog.LogAttrs(ctx, slog.LevelWarn, "S3 request failed",
			slog.String("operation", operation),
			slog.String("key", key),
			slog.Int("status", status),
			slog.String("client_addr", r.RemoteAddr),
			logfmt.Err(err))
	}
	span.SetAttributes(attribute.Int("http.status_code", status))

	s.auditRequest(ctx, r, &auditEntry{
		method:       method,
		bucket:       bucket,
		key:          key,
		operation:    operation,
		status:       status,
		requestSize:  requestSize,
		responseSize: responseSize,
		elapsed:      time.Since(start),
		err:          err,
	})
}

// rejectAuth writes a 403 AccessDenied response after authentication
// failure and emits the matching audit log.
func (s *Server) rejectAuth(ctx context.Context, w http.ResponseWriter, r *http.Request, method string, start time.Time, err error) {
	s.recordRequest(method, http.StatusForbidden, start, 0, 0)
	slog.LogAttrs(ctx, slog.LevelWarn, "S3 auth failure",
		slog.String("method", method),
		slog.String("path", r.URL.Path),
		slog.String("client_addr", r.RemoteAddr),
		logfmt.Err(err))
	audit.Log(ctx, "s3.AuthFailure",
		slog.String("method", method),
		slog.String("path", r.URL.Path),
		slog.String("client_addr", r.RemoteAddr),
		logfmt.Err(err),
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
		observe.RecordSpanError(span, err)
	}
	span.SetAttributes(attribute.Int("http.status_code", status))
	audit.Log(ctx, "s3.ListBuckets",
		slog.String("operation", "ListBuckets"),
		slog.String("method", method),
		slog.String("path", r.URL.Path),
		slog.String("client_addr", r.RemoteAddr),
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
		slog.String("client_addr", r.RemoteAddr),
		slog.Int("status", http.StatusBadRequest),
		slog.Duration("duration", time.Since(start)),
	)
	writeS3Error(w, http.StatusBadRequest, "InvalidRequest", "Invalid path format")
}

// rejectBucketMismatch writes a 403 AccessDenied when the path bucket
// does not match the bucket the credentials authorize.
func (s *Server) rejectBucketMismatch(ctx context.Context, w http.ResponseWriter, r *http.Request, method string, start time.Time, authorizedBucket, bucket string) {
	s.recordRequest(method, http.StatusForbidden, start, 0, 0)
	s.logger().WarnContext(ctx, "bucket mismatch",
		"method", method,
		"path", r.URL.Path,
		"client_addr", r.RemoteAddr,
		"authorized", authorizedBucket,
		"requested", bucket,
	)
	audit.Log(ctx, "s3.BucketMismatch",
		slog.String("method", method),
		slog.String("path", r.URL.Path),
		slog.String("client_addr", r.RemoteAddr),
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

// objectRouteKey carries the path-derived identifiers a per-object
// dispatcher needs.
type objectRouteKey struct {
	method      string
	bucket      string
	key         string
	internalKey string
	uploadID    string
}

// routeObjectRequest dispatches object-level operations to their
// handlers, splitting on multipart-upload state. supported=false means
// the method/query combination is not supported and the caller emits 405.
func (s *Server) routeObjectRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, method, bucket, key, internalKey string) (string, int, int64, int64, error, bool) {
	query := r.URL.Query()

	// Refuse before dispatch. S3 selects the operation from the query string,
	// so an unrecognised key names an operation this server does not
	// implement; falling through would run PutObject or DeleteObject against
	// the key instead, overwriting or removing the object the caller was
	// asking about.
	if sub, unsupported := unsupportedObjectQuery(query); unsupported {
		msg := fmt.Sprintf("object subresource %q is not supported", sub)
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented", msg)
		return "UnsupportedSubresource", http.StatusNotImplemented, 0, 0, nil, true
	}

	// Checked ahead of the multipart split because a tagging request carries
	// neither uploads nor uploadId, so it would otherwise fall through to
	// PutObject, GetObject or DeleteObject against the key it names.
	if _, hasTagging := query["tagging"]; hasTagging {
		return s.routeTaggingRequest(ctx, w, r, method, internalKey)
	}

	_, hasUploads := query["uploads"]
	uploadID := query.Get("uploadId")
	rk := &objectRouteKey{method: method, bucket: bucket, key: key, internalKey: internalKey, uploadID: uploadID}

	switch {
	case hasUploads && method == http.MethodPost:
		st, e := s.handleCreateMultipartUpload(ctx, w, r, bucket, key, internalKey)
		return "CreateMultipartUpload", st, 0, 0, e, true
	case uploadID != "":
		return s.routeMultipartRequest(ctx, w, r, rk)
	default:
		return s.routePlainObjectRequest(ctx, w, r, method, bucket, internalKey)
	}
}

// routeMultipartRequest dispatches per-uploadID multipart operations.
func (s *Server) routeMultipartRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, rk *objectRouteKey) (string, int, int64, int64, error, bool) {
	switch rk.method {
	case http.MethodPut:
		st, e := s.handleUploadPart(ctx, w, r, rk.bucket, rk.key)
		return "UploadPart", st, r.ContentLength, 0, e, true
	case http.MethodPost:
		st, e := s.handleCompleteMultipartUpload(ctx, w, r, rk.bucket, rk.key)
		return "CompleteMultipartUpload", st, 0, 0, e, true
	case http.MethodDelete:
		st, e := s.handleAbortMultipartUpload(ctx, w, rk.bucket, rk.key, rk.uploadID)
		return "AbortMultipartUpload", st, 0, 0, e, true
	case http.MethodGet:
		st, e := s.handleListParts(ctx, w, r, rk.bucket, rk.key, rk.internalKey)
		return "ListParts", st, 0, 0, e, true
	}
	return "", 0, 0, 0, nil, false
}

// routeTaggingRequest dispatches the three ?tagging subresource operations.
// supported=false for any other method, which the caller renders as 405 rather
// than letting it reach the object itself.
func (s *Server) routeTaggingRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, method, internalKey string) (string, int, int64, int64, error, bool) {
	switch method {
	case http.MethodGet:
		st, e := s.handleGetObjectTagging(ctx, w, internalKey)
		return "GetObjectTagging", st, 0, 0, e, true
	case http.MethodPut:
		st, e := s.handlePutObjectTagging(ctx, w, r, internalKey)
		return "PutObjectTagging", st, r.ContentLength, 0, e, true
	case http.MethodDelete:
		st, e := s.handleDeleteObjectTagging(ctx, w, internalKey)
		return "DeleteObjectTagging", st, 0, 0, e, true
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
			st, e := s.handleCopyObject(ctx, w, bucket, internalKey, copySource)
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

// auditEntry carries the data emitted to the audit log for a completed
// S3 request. Bundling these keeps auditRequest under the parameter
// count limit.
type auditEntry struct {
	method       string
	bucket       string
	key          string
	operation    string
	status       int
	requestSize  int64
	responseSize int64
	elapsed      time.Duration
	err          error
}

// auditRequest emits the per-request audit log entry. Path, method,
// bucket, status, and duration are always present; key, sizes, and
// error are appended only when they carry information.
func (s *Server) auditRequest(ctx context.Context, r *http.Request, e *auditEntry) {
	attrs := []slog.Attr{
		slog.String("operation", e.operation),
		slog.String("method", e.method),
		slog.String("path", r.URL.Path),
		slog.String("client_addr", r.RemoteAddr),
		slog.String("bucket", e.bucket),
		slog.Int("status", e.status),
		slog.Duration("duration", e.elapsed),
	}
	if e.key != "" {
		attrs = append(attrs, slog.String("key", e.key))
	}
	if e.requestSize > 0 {
		attrs = append(attrs, slog.Int64("request_size", e.requestSize))
	}
	if e.responseSize > 0 {
		attrs = append(attrs, slog.Int64("response_size", e.responseSize))
	}
	if e.err != nil {
		attrs = append(attrs, slog.String("error", e.err.Error()))
	}
	audit.Log(ctx, "s3."+e.operation, attrs...)
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

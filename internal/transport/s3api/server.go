// -------------------------------------------------------------------------------
// HTTP Server - S3 Request Routing
//
// Author: Alex Freidah
//
// HTTP server and request router for S3-compatible operations. Implements a
// subset of the S3 API sufficient for basic object storage: PUT, GET, HEAD,
// DELETE. Each request is settled into a target naming its action before
// anything runs, authorized against that action, and dispatched by one switch.
// Supports multi-bucket isolation via credential-based bucket resolution and
// internal key prefixing.
// -------------------------------------------------------------------------------

package s3api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/util/must"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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
	PutObject(ctx context.Context, req *object.PutObjectRequest) (string, error)
	GetObject(ctx context.Context, key, rangeHeader string) (*object.GetResult, error)
	HeadObject(ctx context.Context, key string) (*object.HeadResult, error)
	DeleteObject(ctx context.Context, key string) error
	DeleteObjects(ctx context.Context, keys []string) []object.DeleteObjectResult
	CopyObject(ctx context.Context, req *object.CopyObjectRequest) (string, error)
	ListObjects(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*core.ListObjectsResult, error)
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
	CreateMultipartUpload(ctx context.Context, req *multipart.CreateUploadRequest) (string, string, error)
	UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, body io.Reader, size int64) (string, error)
	CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, manifest []core.CompletePart) (string, error)
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error
	ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]core.MultipartUpload, error)
	GetParts(ctx context.Context, bucket, key, uploadID string) ([]core.MultipartPart, error)
	ListParts(ctx context.Context, bucket, key, uploadID string, partNumberMarker, maxParts int) ([]core.MultipartPart, bool, error)
	CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error)
}

// Compile-time assertions.
var (
	_ ObjectOps    = (*object.Manager)(nil)
	_ MultipartOps = (*multipart.Manager)(nil)
)

// Server handles HTTP requests and routes them to the object and multipart
// managers. MaxObjectSize is the upload body ceiling in bytes, zero for none;
// startedAt is the stable CreationDate ListBuckets reports.
type Server struct {
	Objects       ObjectOps
	Multipart     MultipartOps
	MaxObjectSize int64
	registry      *auth.LiveRegistry
	startedAt     time.Time
	log           *slog.Logger
}

// NewServer creates a Server authenticating against the registry in force and
// stamped with a stable start time.
func NewServer(objects ObjectOps, multipartMgr MultipartOps, registry *auth.LiveRegistry, maxObjectSize int64) *Server {
	must.NotNil("objects", objects)
	must.NotNil("multipart", multipartMgr)
	must.NotNil("registry", registry)
	return &Server{
		Objects:       objects,
		Multipart:     multipartMgr,
		MaxObjectSize: maxObjectSize,
		registry:      registry,
		startedAt:     time.Now(),
		log:           slog.Default().With(logfmt.Component("s3_server")),
	}
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	requestID := r.Header.Get("X-Request-Id")
	if !isValidRequestID(requestID) {
		requestID = audit.NewID()
	}
	ctx := audit.WithWorkload(audit.WithRequestID(r.Context(), requestID), audit.WorkloadClient)
	w.Header().Set("X-Amz-Request-Id", requestID)

	telemetry.InflightRequests.WithLabelValues(r.Method).Inc()
	defer telemetry.InflightRequests.WithLabelValues(r.Method).Dec()

	user, streamMat, err := s.registry.Load().Authenticate(r)
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "S3 auth failure", logfmt.Request(r, logfmt.Err(err))...)
		s.reject(ctx, w, r, start, accessDenied("s3.AuthFailure", logfmt.Err(err)))
		return
	}
	// Set before any work happens, so every audit entry this request produces
	// names the identity behind it - including the storage-layer one written
	// several packages deeper, which is handed a context and nothing else.
	ctx = audit.WithUser(ctx, user.ID)
	if streamMat != nil {
		applyStreamingBody(r, streamMat)
	}

	// Named before dispatch rather than reported after it. An authorization
	// check has nowhere to sit if the operation is only known once the handler
	// has already written the response.
	t, ok := newTarget(r)
	if !ok {
		s.reject(ctx, w, r, start, &refusal{
			status: http.StatusBadRequest, event: "s3.InvalidPath", code: "InvalidRequest", msg: "Invalid path format",
		})
		return
	}
	if t.bucket != "" && !s.authorize(ctx, w, r, start, user, t) {
		return
	}

	ctx, span := telemetry.StartServerSpan(ctx, spanName(r.Method),
		append(telemetry.RequestAttributes(r.Method, r.URL.Path, t.bucket, t.key, r.RemoteAddr),
			telemetry.AttrRequestID.String(requestID))...,
	)
	defer span.End()
	s.serve(ctx, span, w, r, &served{start: start, user: user, target: t})
}

// served carries a request through its handler and the record written after:
// when it started, who sent it, what it addresses, and what the handler did.
type served struct {
	start        time.Time
	user         *auth.User
	target       *target
	status       int
	requestSize  int64
	responseSize int64
	err          error
}

// serve dispatches an authorized request, writes any failure its handler left
// unwritten, and records the outcome in metrics, the span, the log and the
// audit trail.
func (s *Server) serve(ctx context.Context, span trace.Span, w http.ResponseWriter, r *http.Request, out *served) {
	if !s.dispatch(ctx, w, r, out) {
		msg := "Method not supported"
		if out.target.key == "" {
			msg = "Method not supported for bucket"
		}
		s.reject(ctx, w, r, out.start, &refusal{
			status: http.StatusMethodNotAllowed, event: "s3.MethodNotAllowed", code: "MethodNotAllowed", msg: msg,
			attrs: []slog.Attr{slog.String("bucket", out.target.bucket)},
		})
		observe.MarkSpanError(span, msg)
		return
	}
	if out.status == 0 && out.err != nil {
		out.status = fail(w, out.err)
	}

	s.recordRequest(r.Method, out.status, out.start, out.requestSize, out.responseSize)
	if out.err != nil {
		observe.RecordSpanError(span, out.err)
		s.log.LogAttrs(ctx, slog.LevelWarn, "S3 request failed", logfmt.Request(r,
			slog.String("operation", string(out.target.act)),
			slog.String("key", out.target.key),
			slog.Int("status", out.status),
			logfmt.Err(out.err))...)
	}
	span.SetAttributes(attribute.Int("http.status_code", out.status))
	s.auditRequest(ctx, r, out)
}

// dispatch runs the handler for the request's action, recording what it did on
// out. A handler returns the status it wrote, or zero with the error serve
// writes for it. Reports false for a method and query combination this server
// does not implement, which the caller answers 405.
func (s *Server) dispatch(ctx context.Context, w http.ResponseWriter, r *http.Request, out *served) bool {
	t := out.target
	switch t.act {
	case ActionListBuckets:
		out.status, out.err = s.handleListBuckets(w, out.user.Buckets())
	case ActionHeadBucket:
		out.status, out.err = s.handleHeadBucket(w)
	case ActionGetBucketVersioning:
		out.status, out.err = respondXML(w, xmlVersioningConfiguration{})
	case ActionGetBucketLocation:
		out.status, out.err = respondXML(w, xmlLocationConstraint{})
	case ActionListMultipartUpload:
		out.status, out.err = s.handleListMultipartUploads(ctx, w, t)
	case ActionListObjectsV2:
		out.status, out.err = s.handleListObjectsV2(ctx, w, t)
	case ActionListObjectsV1:
		out.status, out.err = s.handleListObjectsV1(ctx, w, t)
	case ActionDeleteObjects:
		out.status, out.err = s.handleDeleteObjects(ctx, w, r, t)
	case ActionPutObject:
		out.requestSize = r.ContentLength
		out.status, out.err = s.handlePut(ctx, w, r, t)
	case ActionCopyObject:
		out.status, out.err = s.handleCopyObject(ctx, w, r, t)
	case ActionGetObject:
		out.status, out.responseSize, out.err = s.handleGet(ctx, w, r, t)
	case ActionHeadObject:
		out.status, out.err = s.handleHead(ctx, w, r, t)
	case ActionDeleteObject:
		out.status, out.err = s.handleDelete(ctx, w, t)
	case ActionCreateMultipartUpload:
		out.status, out.err = s.handleCreateMultipartUpload(ctx, w, r, t)
	case ActionUploadPart:
		out.requestSize = r.ContentLength
		out.status, out.err = s.handleUploadPart(ctx, w, r, t)
	case ActionUploadPartCopy:
		out.status, out.err = s.handleUploadPartCopy(ctx, w, r, t)
	case ActionCompleteMultipartUpload:
		out.status, out.err = s.handleCompleteMultipartUpload(ctx, w, r, t)
	case ActionAbortMultipartUpload:
		out.status, out.err = s.handleAbortMultipartUpload(ctx, w, t)
	case ActionListParts:
		out.status, out.err = s.handleListParts(ctx, w, t)
	case ActionGetObjectTagging:
		out.status, out.err = s.handleGetObjectTagging(ctx, w, t)
	case ActionPutObjectTagging:
		out.requestSize = r.ContentLength
		out.status, out.err = s.handlePutObjectTagging(ctx, w, r, t)
	case ActionDeleteObjectTagging:
		out.status, out.err = s.handleDeleteObjectTagging(ctx, w, t)
	case ActionUnsupportedSubresource:
		out.err = unsupportedSubresource(t)
	default:
		return false
	}
	return true
}

// unsupportedSubresource refuses a query naming a subresource this server does
// not implement. Answering it as the data operation its method implies would
// read as an empty listing, or overwrite or delete the object.
func unsupportedSubresource(t *target) error {
	scope, allowed, prefixes := "object", supportedObjectQueryKeys, supportedObjectQueryPrefixes
	if t.key == "" {
		scope, allowed, prefixes = "bucket", supportedBucketQueryKeys, supportedBucketQueryPrefixes
	}
	sub, _ := unsupportedQuery(t.query, allowed, prefixes)
	return s3Err(http.StatusNotImplemented, "NotImplemented", fmt.Sprintf("%s subresource %q is not supported", scope, sub))
}

// spanName names the server span for an HTTP method.
func spanName(method string) string {
	if name, ok := httpSpanName[method]; ok {
		return name
	}
	return "HTTP " + method
}

// -------------------------------------------------------------------------
// REFUSALS
// -------------------------------------------------------------------------

// refusal is a request refused before it reaches a handler: the status and S3
// error it answers with, and the audit event with the fields it records beyond
// the request's own. An empty event means the refusal was already recorded.
type refusal struct {
	status int
	event  string
	code   string
	msg    string
	attrs  []slog.Attr
}

// accessDenied is the refusal for event as a 403, the answer S3 gives whether
// the credential failed or its grant did not reach.
func accessDenied(event string, attrs ...slog.Attr) *refusal {
	return &refusal{status: http.StatusForbidden, event: event, code: s3CodeAccessDenied, msg: "Access denied", attrs: attrs}
}

// reject records, audits and answers a refused request. Every refusal's audit
// entry carries the method, path and client address first and the status and
// duration last, so one query reads them all the same way.
func (s *Server) reject(ctx context.Context, w http.ResponseWriter, r *http.Request, start time.Time, f *refusal) {
	s.recordRequest(r.Method, f.status, start, 0, 0)
	if f.event != "" {
		audit.Log(ctx, f.event, logfmt.Request(r, append(f.attrs,
			slog.Int("status", f.status),
			slog.Duration("duration", time.Since(start)),
		)...)...)
	}
	WriteS3Error(w, f.status, f.code, f.msg)
}

// authorize refuses a caller whose grants do not carry what the action needs on
// the bucket, deciding as every other surface does. A caller with no grant on
// the bucket is filed as s3.BucketDenied, one whose grant lacks a permission as
// s3.ActionDenied. Reports whether the request may proceed.
func (s *Server) authorize(ctx context.Context, w http.ResponseWriter, r *http.Request, start time.Time, user *auth.User, t *target) bool {
	want := RequiredPermissions(t.act)
	reason, ok := auth.AuthorizeBucket(user, t.bucket, want)
	if ok {
		return true
	}
	event := "s3.ActionDenied"
	if reason == auth.ReasonNoGrant {
		event = "s3.BucketDenied"
	}
	denial := &auth.Denial{
		Event: event, User: user, Operation: string(t.act),
		Resource: core.Resource{Kind: core.ResourceBucket, Name: t.bucket}, Required: want, Reason: reason,
	}
	denial.Record(r.WithContext(ctx), s.log)
	s.reject(ctx, w, r, start, accessDenied(""))
	return false
}

// -------------------------------------------------------------------------
// REQUEST RECORD
// -------------------------------------------------------------------------

// auditRequest emits the per-request audit log entry. The request's method,
// path and client address, its operation, status and duration are always
// present; bucket, key, sizes and error only when they carry information.
func (s *Server) auditRequest(ctx context.Context, r *http.Request, out *served) {
	t := out.target
	attrs := logfmt.Request(r,
		slog.String("operation", string(t.act)),
		slog.Int("status", out.status),
		slog.Duration("duration", time.Since(out.start)),
	)
	if t.bucket != "" {
		attrs = append(attrs, slog.String("bucket", t.bucket))
	}
	if t.key != "" {
		attrs = append(attrs, slog.String("key", t.key))
	}
	if out.requestSize > 0 {
		attrs = append(attrs, slog.Int64("request_size", out.requestSize))
	}
	if out.responseSize > 0 {
		attrs = append(attrs, slog.Int64("response_size", out.responseSize))
	}
	if out.err != nil {
		attrs = append(attrs, logfmt.Err(out.err))
	}
	audit.Log(ctx, "s3."+string(t.act), attrs...)
}

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

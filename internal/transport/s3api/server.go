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

package s3api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

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
	PutObject(ctx context.Context, req *object.PutObjectRequest) (string, error)
	GetObject(ctx context.Context, key, rangeHeader string) (*object.GetResult, error)
	HeadObject(ctx context.Context, key string) (*object.HeadResult, error)
	DeleteObject(ctx context.Context, key string) error
	DeleteObjects(ctx context.Context, keys []string) []object.DeleteObjectResult
	CopyObject(ctx context.Context, req *object.CopyObjectRequest) (string, error)
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
	CreateMultipartUpload(ctx context.Context, req *multipart.CreateUploadRequest) (string, string, error)
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

	user, streamMat, err := s.GetBucketAuth().Authenticate(r)
	if err != nil {
		s.rejectAuth(ctx, w, r, method, start, err)
		return
	}
	// Set before any work happens, so every audit entry this request produces
	// names the identity behind it - including the storage-layer one written
	// several packages deeper, which is handed a context and nothing else.
	ctx = audit.WithUser(ctx, user.ID)
	if streamMat != nil {
		applyStreamingBody(r, streamMat)
	}

	if r.URL.Path == "/" && method == http.MethodGet {
		s.serveListBuckets(ctx, w, r, method, requestID, start, user)
		return
	}

	bucket, key, ok := parsePath(r.URL.Path)
	if !ok {
		s.rejectInvalidPath(ctx, w, r, method, start)
		return
	}
	// Named before dispatch rather than reported after it. An authorization
	// check has nowhere to sit if the operation is only known once the handler
	// has already written the response.
	act := Classify(r, key)
	if !s.authorize(ctx, w, r, &authRequest{
		method: method, start: start, user: user, bucket: bucket, act: act,
	}) {
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

	var res routed
	if key == "" {
		res, err = s.routeBucketRequest(ctx, w, r, act, bucket)
	} else {
		res, err = s.routeObjectRequest(ctx, w, r, act, bucket, key, internalKey)
	}
	if !res.supported {
		if key == "" {
			rejectMethod("Method not supported for bucket")
		} else {
			rejectMethod("Method not supported")
		}
		return
	}

	s.recordRequest(method, res.status, start, res.requestSize, res.responseSize)

	if err != nil {
		observe.RecordSpanError(span, err)
		slog.LogAttrs(ctx, slog.LevelWarn, "S3 request failed",
			slog.String("operation", res.operation),
			slog.String("key", key),
			slog.Int("status", res.status),
			slog.String("client_addr", r.RemoteAddr),
			logfmt.Err(err))
	}
	span.SetAttributes(attribute.Int("http.status_code", res.status))

	s.auditRequest(ctx, r, &auditEntry{
		method:       method,
		bucket:       bucket,
		key:          key,
		operation:    res.operation,
		status:       res.status,
		requestSize:  res.requestSize,
		responseSize: res.responseSize,
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
	writeAccessDenied(w)
}

// serveListBuckets handles the special-case GET / route, which enumerates every
// bucket the caller's identity reaches.
func (s *Server) serveListBuckets(ctx context.Context, w http.ResponseWriter, r *http.Request, method, requestID string, start time.Time, user *auth.User) {
	ctx, span := telemetry.StartServerSpan(ctx, "HTTP GET",
		append(telemetry.RequestAttributes(method, "/", "", "", r.RemoteAddr),
			telemetry.AttrRequestID.String(requestID))...,
	)
	defer span.End()

	status, err := s.handleListBuckets(w, user.Buckets())
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

// rejectBucketDenied writes a 403 AccessDenied when the authenticated user holds
// no grant on the bucket the path names.
func (s *Server) rejectBucketDenied(ctx context.Context, w http.ResponseWriter, r *http.Request, method string, start time.Time, user *auth.User, bucket string) {
	s.recordRequest(method, http.StatusForbidden, start, 0, 0)
	s.logger().WarnContext(ctx, "bucket not granted",
		"method", method,
		"path", r.URL.Path,
		"client_addr", r.RemoteAddr,
		"user", user.ID,
		"requested", bucket,
	)
	audit.Log(ctx, "s3.BucketDenied",
		slog.String("method", method),
		slog.String("path", r.URL.Path),
		slog.String("client_addr", r.RemoteAddr),
		slog.String("requested_bucket", bucket),
		slog.Int("status", http.StatusForbidden),
		slog.Duration("duration", time.Since(start)),
	)
	writeAccessDenied(w)
}

// authRequest carries what an authorization decision reads about a request.
// Bundled because the two refusals below need the same set and passing them
// positionally at three call sites is where they get transposed.
type authRequest struct {
	method string
	start  time.Time
	user   *auth.User
	bucket string
	act    Action
}

// authorize refuses a caller that does not reach the bucket, and one whose
// grant does not carry what the operation needs. Reports whether the request
// may proceed; the refusal is already written when it may not.
//
// Both checks live here so the request path has one authorization step rather
// than two spread either side of the span. They stay separate refusals: a
// bucket the caller was never granted and an operation its grant does not allow
// are different states an operator fixes differently.
func (s *Server) authorize(ctx context.Context, w http.ResponseWriter, r *http.Request, a *authRequest) bool {
	if !a.user.CanReach(a.bucket) {
		s.rejectBucketDenied(ctx, w, r, a.method, a.start, a.user, a.bucket)
		return false
	}
	if want := RequiredPermissions(a.act); !a.user.Can(a.bucket, want) {
		s.rejectActionDenied(ctx, w, r, a, want)
		return false
	}
	return true
}

// rejectActionDenied writes a 403 for a caller that reaches the bucket but
// whose grant does not carry what the operation needs.
//
// Recorded as its own audit event rather than folded into the bucket refusal,
// because the two describe different states an operator acts on differently:
// one is a client pointed at a bucket it was never granted, the other a client
// granted the bucket and asking for more than its grant allows. The entry names
// what was needed and what was held so the fix is readable without a second
// lookup.
func (s *Server) rejectActionDenied(ctx context.Context, w http.ResponseWriter, r *http.Request, a *authRequest, want core.PermissionSet) {
	held, _ := a.user.Permissions(a.bucket)
	s.recordRequest(a.method, http.StatusForbidden, a.start, 0, 0)
	s.logger().WarnContext(ctx, "operation not permitted by grant",
		"method", a.method,
		"path", r.URL.Path,
		"client_addr", r.RemoteAddr,
		"user", a.user.ID,
		"bucket", a.bucket,
		"operation", string(a.act),
	)
	audit.Log(ctx, "s3.ActionDenied",
		slog.String("method", a.method),
		slog.String("path", r.URL.Path),
		slog.String("client_addr", r.RemoteAddr),
		slog.String("bucket", a.bucket),
		slog.String("operation", string(a.act)),
		slog.String("required", want.String()),
		slog.String("held", held.String()),
		slog.Int("status", http.StatusForbidden),
		slog.Duration("duration", time.Since(a.start)),
	)
	writeAccessDenied(w)
}

// routed is what a dispatcher reports about the request it handled: the
// operation name metrics and the audit log are keyed on, the status written,
// the bytes in and out, and whether the method and query combination is one
// this server implements.
//
// A struct rather than six positional results. The dispatchers return the same
// shape at twenty-odd sites, and half of those results are zero at any given
// one; naming them is what makes a return statement readable without counting
// commas back to the signature.
type routed struct {
	operation    string
	status       int
	requestSize  int64
	responseSize int64
	supported    bool
}

// routeBucketRequest dispatches bucket-level operations (no object key)
// to their handlers. supported=false means none of the supported method/
// query combinations matched; the caller emits a 405.
func (s *Server) routeBucketRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, act Action, bucket string) (routed, error) {
	switch act {
	// Refused before dispatch, for the reason the object path does: S3 selects
	// the operation from the query string, so an unrecognised key names a
	// bucket subresource this server does not implement. Falling through
	// answers a ListBucketResult, which a client that asked for versions, a
	// policy or a lifecycle configuration parses as "there are none".
	case ActionUnsupportedSubresource:
		sub, _ := unsupportedQuery(r.URL.Query(), supportedBucketQueryKeys, supportedBucketQueryPrefixes)
		msg := fmt.Sprintf("bucket subresource %q is not supported", sub)
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented", msg)
		return done(act, http.StatusNotImplemented), nil
	case ActionHeadBucket:
		st, e := s.handleHeadBucket(w)
		return done(act, st), e
	case ActionGetBucketVersioning:
		st, e := s.handleGetBucketVersioning(w)
		return done(act, st), e
	case ActionListMultipartUpload:
		st, e := s.handleListMultipartUploads(ctx, w, r, bucket)
		return done(act, st), e
	case ActionGetBucketLocation:
		st, e := s.handleGetBucketLocation(w)
		return done(act, st), e
	case ActionListObjectsV2:
		st, e := s.handleListObjectsV2(ctx, w, r, bucket)
		return done(act, st), e
	case ActionListObjectsV1:
		st, e := s.handleListObjectsV1(ctx, w, r, bucket)
		return done(act, st), e
	case ActionDeleteObjects:
		st, e := s.handleDeleteObjects(ctx, w, r, bucket)
		return done(act, st), e
	}
	return routed{}, nil
}

// done builds the result for a dispatched action. The operation name is the
// action itself rather than a literal repeated at every return, so the two
// cannot drift.
func done(act Action, status int) routed {
	return routed{operation: string(act), status: status, supported: true}
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
func (s *Server) routeObjectRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, act Action, bucket, key, internalKey string) (routed, error) {
	// Refused before dispatch. S3 selects the operation from the query string,
	// so an unrecognised key names an operation this server does not
	// implement; falling through would run PutObject or DeleteObject against
	// the key instead, overwriting or removing the object the caller was
	// asking about.
	if act == ActionUnsupportedSubresource {
		sub, _ := unsupportedQuery(r.URL.Query(), supportedObjectQueryKeys, supportedObjectQueryPrefixes)
		msg := fmt.Sprintf("object subresource %q is not supported", sub)
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented", msg)
		return done(act, http.StatusNotImplemented), nil
	}

	if act == ActionCreateMultipartUpload {
		st, e := s.handleCreateMultipartUpload(ctx, w, r, bucket, key, internalKey)
		return done(act, st), e
	}

	rk := &objectRouteKey{
		method:      r.Method,
		bucket:      bucket,
		key:         key,
		internalKey: internalKey,
		uploadID:    r.URL.Query().Get("uploadId"),
	}
	if res, ok, err := s.routeTaggingRequest(ctx, w, r, act, internalKey); ok {
		return res, err
	}
	if res, ok, err := s.routeMultipartRequest(ctx, w, r, act, rk); ok {
		return res, err
	}
	return s.routePlainObjectRequest(ctx, w, r, act, bucket, internalKey)
}

// routeMultipartRequest dispatches per-uploadID multipart operations. PUT
// splits between UploadPart and UploadPartCopy on the X-Amz-Copy-Source
// header, the same split routePlainObjectRequest makes: an UploadPartCopy
// carries no body, so handing it to UploadPart stores an empty part.
// ok reports false when the action belongs to another dispatcher, so the
// caller tries the next one.
func (s *Server) routeMultipartRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, act Action, rk *objectRouteKey) (res routed, ok bool, err error) {
	switch act {
	case ActionUploadPartCopy:
		st, e := s.handleUploadPartCopy(ctx, w, r, rk, r.Header.Get(headerCopySource))
		return done(act, st), true, e
	case ActionUploadPart:
		st, e := s.handleUploadPart(ctx, w, r, rk.bucket, rk.key)
		out := done(act, st)
		out.requestSize = r.ContentLength
		return out, true, e
	case ActionCompleteMultipartUpload:
		st, e := s.handleCompleteMultipartUpload(ctx, w, r, rk.bucket, rk.key)
		return done(act, st), true, e
	case ActionAbortMultipartUpload:
		st, e := s.handleAbortMultipartUpload(ctx, w, rk.bucket, rk.key, rk.uploadID)
		return done(act, st), true, e
	case ActionListParts:
		st, e := s.handleListParts(ctx, w, r, rk.bucket, rk.key, rk.internalKey)
		return done(act, st), true, e
	}
	return routed{}, false, nil
}

// routeTaggingRequest dispatches the three ?tagging subresource operations.
// supported=false for any other method, which the caller renders as 405 rather
// than letting it reach the object itself.
// ok reports false when the action is not a tagging one, so the caller tries
// the next dispatcher.
func (s *Server) routeTaggingRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, act Action, internalKey string) (res routed, ok bool, err error) {
	switch act {
	case ActionGetObjectTagging:
		st, e := s.handleGetObjectTagging(ctx, w, internalKey)
		return done(act, st), true, e
	case ActionPutObjectTagging:
		st, e := s.handlePutObjectTagging(ctx, w, r, internalKey)
		out := done(act, st)
		out.requestSize = r.ContentLength
		return out, true, e
	case ActionDeleteObjectTagging:
		st, e := s.handleDeleteObjectTagging(ctx, w, internalKey)
		return done(act, st), true, e
	}
	return routed{}, false, nil
}

// routePlainObjectRequest dispatches non-multipart object operations.
// PUT splits between PutObject and CopyObject based on the
// X-Amz-Copy-Source header.
func (s *Server) routePlainObjectRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, act Action, bucket, internalKey string) (routed, error) {
	switch act {
	case ActionCopyObject:
		st, e := s.handleCopyObject(ctx, w, r, bucket, internalKey, r.Header.Get(headerCopySource))
		return done(act, st), e
	case ActionPutObject:
		st, e := s.handlePut(ctx, w, r, internalKey)
		out := done(act, st)
		out.requestSize = r.ContentLength
		return out, e
	case ActionGetObject:
		st, sz, e := s.handleGet(ctx, w, r, internalKey)
		out := done(act, st)
		out.responseSize = sz
		return out, e
	case ActionHeadObject:
		st, e := s.handleHead(ctx, w, r, internalKey)
		return done(act, st), e
	case ActionDeleteObject:
		st, e := s.handleDelete(ctx, w, r, internalKey)
		return done(act, st), e
	}
	return routed{}, nil
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

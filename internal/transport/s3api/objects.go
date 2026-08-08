// -------------------------------------------------------------------------------
// Object Handlers - PUT, GET, HEAD, DELETE, COPY, DeleteObjects
//
// Author: Alex Freidah
//
// HTTP handlers for standard S3 object operations. Streams response bodies
// directly to avoid buffering large objects in memory. DeleteObjects handles
// batch deletion of up to 1000 keys per request with concurrent backend I/O.
// -------------------------------------------------------------------------------

package s3api

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
)

// errIfNoneMatchExists is returned by handlePut when the request carries
// `If-None-Match: *` and a location row already exists for the target key.
// Used so the audit log captures a typed reason for the 412 response.
var errIfNoneMatchExists = errors.New("If-None-Match: * precondition failed: object exists")

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

// deleteObjectEntry is one key in a DeleteObjects request body. The S3
// XML schema also allows a per-key VersionId, which the orchestrator
// ignores because object versioning is not implemented.
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

// deletedObject is one entry in the Deleted block of a DeleteObjects
// response. Suppressed when the request had Quiet=true.
type deletedObject struct {
	Key string `xml:"Key"`
}

// deleteObjectError is one entry in the Error block of a DeleteObjects
// response. Always emitted on per-key failure even when Quiet=true so
// the client can distinguish a partial failure from a full success.
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
	if status, err, ok := enforceContentLength(w, r, s.MaxObjectSize, "Object"); !ok {
		return status, err
	}

	contentType := r.Header.Get(headerContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	metadata := extractUserMetadata(r.Header)
	if len(metadata) > 0 {
		if err := validateUserMetadata(metadata); err != nil {
			writeS3Error(w, http.StatusBadRequest, "MetadataTooLarge", err.Error())
			return http.StatusBadRequest, err
		}
	}

	// --- Conditional write: If-None-Match: * fails when the key exists ---
	// Best-effort precondition check before the body upload so a doomed
	// request does not transmit. Matches AWS S3's documented behavior:
	// the check is not a hard guarantee under contention, but eliminates
	// the common case of a client trying to avoid clobbering a known key.
	if status, err, done := s.checkIfNoneMatchStar(ctx, w, r, key); done {
		return status, err
	}

	// --- Early rejection before body transmission (Expect: 100-Continue) ---
	// Check backend capacity before reading the request body. When the
	// client sends Expect: 100-continue, Go's net/http delays the 100
	// Continue response until the first r.Body.Read(). Rejecting here
	// avoids transmitting the entire upload body for a doomed request.
	if !s.Objects.CanAcceptWrite(r.ContentLength) {
		telemetry.EarlyRejectionsTotal.Inc()
		msg := fmt.Sprintf("No backend can accept a %d byte upload", r.ContentLength)
		if hint := formatCapacityHint(s.Objects.BackendCapacityStats(ctx)); hint != "" {
			msg += "; backend usage: " + hint
		}
		writeS3Error(w, http.StatusInsufficientStorage, "InsufficientStorage", msg)
		return http.StatusInsufficientStorage, fmt.Errorf("no backend capacity for %d bytes", r.ContentLength)
	}

	// --- Add size to span ---
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		telemetry.AttrObjectSize.Int64(r.ContentLength),
		telemetry.AttrContentType.String(contentType),
	)

	etag, err := s.Objects.PutObject(ctx, key, r.Body, r.ContentLength, contentType, metadata)
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
// buffering large objects in memory. Supports Range requests  -  when the client
// sends a Range header, the response is 206 Partial Content with Content-Range.
func (s *Server) handleGet(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, int64, error) {
	rangeHeader := r.Header.Get("Range")

	result, err := s.Objects.GetObject(ctx, key, rangeHeader)
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

	// Validators go out before the conditional check so a 304 carries the
	// ETag and Last-Modified it would have carried on a 200 (RFC 9110 15.4.5).
	setValidatorHeaders(w, result)

	// Preconditions are evaluated before the Range is considered: a failed
	// precondition aborts the whole request, whether or not only part of the
	// representation was asked for (RFC 9110 13.1). Evaluating these only for
	// unranged GETs let a resumable download splice bytes from a replaced
	// object into the file it had already partly fetched.
	if status, done := checkConditionals(r, result.ETag, result.LastModified); done {
		w.WriteHeader(status)
		return status, 0, nil
	}

	// If-Range asks for the range only while the representation is unchanged,
	// and for the whole object otherwise. The partial body already opened is
	// discarded and the object re-fetched in full, which costs an extra round
	// trip only on the mismatch that would otherwise corrupt the download.
	if rangeHeader != "" && !ifRangeMatches(r, result.ETag, result.LastModified) {
		_ = result.Body.Close()
		full, ferr := s.Objects.GetObject(ctx, key, "")
		if ferr != nil {
			return writeStorageError(w, ferr, "Failed to retrieve object"), 0, ferr
		}
		result = full
		setValidatorHeaders(w, result)
	}

	w.Header().Set(headerContentType, result.ContentType)
	if result.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	}
	w.Header().Set("Accept-Ranges", "bytes")
	for k, v := range result.Metadata {
		w.Header().Set("x-amz-meta-"+k, v)
	}

	status := http.StatusOK
	if result.ContentRange != "" {
		w.Header().Set("Content-Range", result.ContentRange)
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)

	written, copyErr := bufpool.Copy(w, result.Body)
	if copyErr != nil {
		return status, written, fmt.Errorf("error streaming body: %w", copyErr)
	}

	return status, written, nil
}

// handleHead processes HEAD requests.
func (s *Server) handleHead(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, error) {
	result, err := s.Objects.HeadObject(ctx, key)
	if err != nil {
		return writeStorageError(w, err, "Failed to retrieve object metadata"), err
	}

	// --- Add size to span ---
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		telemetry.AttrObjectSize.Int64(result.Size),
		telemetry.AttrContentType.String(result.ContentType),
	)

	w.Header().Set(headerContentType, result.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	if result.ETag != "" {
		w.Header().Set("ETag", result.ETag)
	}
	if !result.LastModified.IsZero() {
		w.Header().Set("Last-Modified", result.LastModified.UTC().Format(http.TimeFormat))
	}
	w.Header().Set("Accept-Ranges", "bytes")
	for k, v := range result.Metadata {
		w.Header().Set("x-amz-meta-"+k, v)
	}

	// --- Conditional request evaluation ---
	if status, done := checkConditionals(r, result.ETag, result.LastModified); done {
		w.WriteHeader(status)
		return status, nil
	}

	w.WriteHeader(http.StatusOK)
	return http.StatusOK, nil
}

// handleDelete processes DELETE requests. The manager treats missing objects as
// success (S3 idempotent delete), so any error returned is a real backend failure.
func (s *Server) handleDelete(ctx context.Context, w http.ResponseWriter, _ *http.Request, key string) (int, error) {
	if err := s.Objects.DeleteObject(ctx, key); err != nil {
		return writeStorageError(w, err, "Failed to delete object"), err
	}

	w.WriteHeader(http.StatusNoContent)
	return http.StatusNoContent, nil
}

// handleCopyObject processes PUT requests with the x-amz-copy-source header.
// Copies an object from the source key to the destination key, potentially
// across backends, with atomic quota tracking. Only same-bucket copies are
// allowed (no cross-bucket copying).
func (s *Server) handleCopyObject(ctx context.Context, w http.ResponseWriter, bucket, destInternalKey, copySource string) (int, error) {
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
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "Cross-bucket copy is not allowed")
		return http.StatusForbidden, fmt.Errorf("cross-bucket copy denied: %s != %s", sourceBucket, bucket)
	}

	// Prefix source key for internal storage
	sourceInternalKey := internalkey.Make(bucket, sourceKey)

	etag, err := s.Objects.CopyObject(ctx, sourceInternalKey, destInternalKey)
	if err != nil {
		return writeStorageError(w, err, "Failed to copy object"), err
	}

	result := copyObjectResult{
		Xmlns:        s3XMLNS,
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
	var req deleteObjectsRequest
	if status, err := decodeXMLBody(w, r, maxDeleteObjectsBody, &req); err != nil {
		return status, fmt.Errorf("delete objects: %w", err)
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
		keys[i] = internalkey.Make(bucket, obj.Key)
	}

	results := s.Objects.DeleteObjects(ctx, keys)

	// Build XML response with per-key outcomes
	resp := deleteObjectsResult{
		Xmlns: s3XMLNS,
	}
	for i, res := range results {
		userKey := req.Objects[i].Key
		if res.Err != nil {
			code, msg := deleteObjectErrorFor(res.Err)
			resp.Errors = append(resp.Errors, deleteObjectError{
				Key:     userKey,
				Code:    code,
				Message: msg,
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

// deleteObjectErrorFor extracts the S3-compatible error code and message for
// a per-key DeleteObjects failure. Typed *store.S3Error values surface their
// canonical Code/Message (e.g. NoSuchKey, ServiceUnavailable); generic errors
// fall back to InternalError so clients still see a valid S3 error envelope.
func deleteObjectErrorFor(err error) (code, message string) {
	if s3err, ok := errors.AsType[*core.S3Error](err); ok {
		return s3err.Code, s3err.Message
	}
	return "InternalError", "Failed to delete object"
}

// -------------------------------------------------------------------------
// CONDITIONAL REQUEST HELPERS
// -------------------------------------------------------------------------

// checkIfNoneMatchStar evaluates a write-side `If-None-Match: *`
// precondition before a body upload starts. Returns (status, err, true)
// when the response has already been written and the caller must
// return immediately, or (0, nil, false) when the request should
// proceed normally. AWS S3 only honors the `*` form for write
// preconditions; specific-etag values are not interpreted on write.
func (s *Server) checkIfNoneMatchStar(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, error, bool) {
	if r.Header.Get("If-None-Match") != "*" {
		return 0, nil, false
	}
	exists, err := s.Objects.ObjectExists(ctx, key)
	if err != nil {
		return writeStorageError(w, err, "Failed to evaluate conditional write"), err, true
	}
	if exists {
		writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed",
			"At least one of the pre-conditions you specified did not hold")
		return http.StatusPreconditionFailed, errIfNoneMatchExists, true
	}
	return 0, nil, false
}

// setValidatorHeaders writes the ETag and Last-Modified that identify the
// representation. Split out because they must also appear on a 304, which
// carries no body and skips the rest of the header block.
func setValidatorHeaders(w http.ResponseWriter, result *s3be.GetObjectResult) {
	if result.ETag != "" {
		w.Header().Set("ETag", result.ETag)
	}
	if !result.LastModified.IsZero() {
		w.Header().Set("Last-Modified", result.LastModified.UTC().Format(http.TimeFormat))
	}
}

// ifRangeMatches reports whether an If-Range validator still describes the
// current representation, which is what decides between serving the requested
// range and serving the whole object.
//
// An absent header means the client did not ask for the conditional form, so
// the range stands. A value parseable as an HTTP date is compared as a
// last-modified validator; anything else is compared as an entity tag. Per
// RFC 9110 13.1.5 the entity-tag comparison is strong, so a weak tag never
// matches: a weak validator cannot safely be used to splice a partial
// response onto bytes already held.
func ifRangeMatches(r *http.Request, etag string, lastModified time.Time) bool {
	ir := r.Header.Get("If-Range")
	if ir == "" {
		return true
	}
	if t, err := http.ParseTime(ir); err == nil {
		return !lastModified.IsZero() && lastModified.Equal(t)
	}
	if strings.HasPrefix(ir, "W/") || etag == "" {
		return false
	}
	return ir == etag
}

// checkConditionals evaluates conditional request headers per RFC 7232.
// Returns the HTTP status to send and whether the caller should
// short-circuit. Evaluation order follows the spec: If-Match,
// If-Unmodified-Since, If-None-Match, If-Modified-Since.
func checkConditionals(r *http.Request, etag string, lastModified time.Time) (int, bool) {
	if status, done := checkIfMatch(r.Header.Get("If-Match"), etag); done {
		return status, true
	}
	if status, done := checkIfUnmodifiedSince(r, lastModified); done {
		return status, true
	}
	inm := r.Header.Get("If-None-Match")
	if status, done := checkIfNoneMatch(inm, etag); done {
		return status, true
	}
	if status, done := checkIfModifiedSince(r.Header.Get("If-Modified-Since"), inm, lastModified); done {
		return status, true
	}
	return 0, false
}

// checkIfMatch returns 412 Precondition Failed when If-Match is present
// and does not equal the current etag (or "*"). The empty-etag case
// short-circuits because S3 treats it as "no current representation",
// which never satisfies a strong validator.
func checkIfMatch(im, etag string) (int, bool) {
	if im == "" || etag == "" {
		return 0, false
	}
	if im != etag && im != "*" {
		return http.StatusPreconditionFailed, true
	}
	return 0, false
}

// checkIfUnmodifiedSince returns 412 Precondition Failed when
// If-Unmodified-Since is present without an If-Match (per RFC 7232,
// If-Match takes precedence) and the resource was modified after the
// header's timestamp.
func checkIfUnmodifiedSince(r *http.Request, lastModified time.Time) (int, bool) {
	ius := r.Header.Get("If-Unmodified-Since")
	if ius == "" || r.Header.Get("If-Match") != "" {
		return 0, false
	}
	t, err := http.ParseTime(ius)
	if err != nil {
		return 0, false
	}
	if !lastModified.IsZero() && lastModified.After(t) {
		return http.StatusPreconditionFailed, true
	}
	return 0, false
}

// checkIfNoneMatch returns 304 Not Modified when If-None-Match is
// present and matches the current etag (or "*"). Used by clients to
// avoid re-downloading unchanged objects.
func checkIfNoneMatch(inm, etag string) (int, bool) {
	if inm == "" || etag == "" {
		return 0, false
	}
	if inm == etag || inm == "*" {
		return http.StatusNotModified, true
	}
	return 0, false
}

// checkIfModifiedSince returns 304 Not Modified when If-Modified-Since
// is present, If-None-Match is absent (per RFC 7232 precedence), and the
// resource has not been modified after the header's timestamp.
func checkIfModifiedSince(ims, inm string, lastModified time.Time) (int, bool) {
	if ims == "" || inm != "" {
		return 0, false
	}
	t, err := http.ParseTime(ims)
	if err != nil {
		return 0, false
	}
	if !lastModified.IsZero() && !lastModified.After(t) {
		return http.StatusNotModified, true
	}
	return 0, false
}

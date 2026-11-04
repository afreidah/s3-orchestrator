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
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
)

// errIfNoneMatchExists refuses a write carrying `If-None-Match: *` to a key
// that already holds an object.
var errIfNoneMatchExists = s3Err(http.StatusPreconditionFailed, "PreconditionFailed",
	"At least one of the pre-conditions you specified did not hold")

// -------------------------------------------------------------------------
// XML TYPES
// -------------------------------------------------------------------------

// copyResult is the XML response for CopyObject and UploadPartCopy, which
// differ only in the root element XMLName carries. UploadPart answers with a
// bare ETag header; the copy forms return the ETag in a document, which is
// what SDKs read the validator out of.
type copyResult struct {
	XMLName      xml.Name
	Xmlns        s3Namespace `xml:"xmlns,attr"`
	ETag         string      `xml:"ETag"`
	LastModified string      `xml:"LastModified"`
}

// respondCopy writes the copy response rooted at root.
func respondCopy(w http.ResponseWriter, root, etag string, modified time.Time) (int, error) {
	return respondXML(w, copyResult{
		XMLName:      xml.Name{Local: root},
		ETag:         etag,
		LastModified: modified.UTC().Format(time.RFC3339),
	})
}

// annotateObject records the object's size and content type on the request span.
func annotateObject(ctx context.Context, size int64, contentType string) {
	trace.SpanFromContext(ctx).SetAttributes(
		telemetry.AttrObjectSize.Int64(size),
		telemetry.AttrContentType.String(contentType),
	)
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
	Xmlns   s3Namespace         `xml:"xmlns,attr"`
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
func (s *Server) handlePut(ctx context.Context, w http.ResponseWriter, r *http.Request, t *target) (int, error) {
	if err := s.acceptBody(w, r, "Object"); err != nil {
		return 0, err
	}
	contentType, metadata, err := uploadHeaders(r)
	if err != nil {
		return 0, err
	}

	// Best-effort precondition check before the body upload so a doomed
	// request does not transmit. Matches AWS S3's documented behavior:
	// the check is not a hard guarantee under contention, but eliminates
	// the common case of a client trying to avoid clobbering a known key.
	if err := s.checkIfNoneMatchStar(ctx, r, t.internalKey); err != nil {
		return 0, err
	}

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
		return 0, s3Err(http.StatusInsufficientStorage, "InsufficientStorage", msg)
	}

	// Refused alongside the capacity check and for the same reason: an
	// unusable tag set rejected after the body lands has already spent the
	// transfer and left an object on a backend to clean up.
	tags, err := parseTaggingHeader(r.Header.Get("x-amz-tagging"))
	if err != nil {
		return 0, err
	}

	annotateObject(ctx, r.ContentLength, contentType)

	etag, err := s.Objects.PutObject(ctx, &object.PutObjectRequest{
		Key:         t.internalKey,
		Body:        r.Body,
		Size:        r.ContentLength,
		ContentType: contentType,
		Metadata:    metadata,
		Tags:        tags,
	})
	if err != nil {
		return 0, err
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
// The bytes sent are returned for the request log.
func (s *Server) handleGet(ctx context.Context, w http.ResponseWriter, r *http.Request, t *target) (int, int64, error) {
	key := t.internalKey
	if status, done := s.answerConditionalFromHead(ctx, w, r, key); done {
		return status, 0, nil
	}

	rangeHeader := r.Header.Get("Range")

	result, err := s.Objects.GetObject(ctx, key, rangeHeader)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = result.Body.Close() }()
	annotateObject(ctx, result.Size, result.ContentType)

	// Validators go out before the conditional check so a 304 carries the
	// ETag and Last-Modified it would have carried on a 200 (RFC 9110 15.4.5).
	setValidatorHeaders(w, result.ETag, result.LastModified)

	// Preconditions are evaluated before the Range is considered, so a failed
	// precondition aborts ranged requests too (RFC 9110 13.1). Otherwise a
	// resumable download could splice bytes from a replaced object.
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
			return 0, 0, ferr
		}
		result = full
		setValidatorHeaders(w, result.ETag, result.LastModified)
	}

	setObjectHeaders(w, result.ContentType, result.Metadata, result.TagCount)
	if result.Size > 0 {
		w.Header().Set(httputil.HeaderContentLength, strconv.FormatInt(result.Size, 10))
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

// answerConditionalFromHead answers a conditional GET whose precondition fails
// from the object's metadata, so a 304 or 412 never opens a backend read.
// Anything it cannot settle - no precondition, one that passes, or a HEAD that
// errs - falls through to the GET, whose own check stays the authority in case
// the object changed in between.
func (s *Server) answerConditionalFromHead(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) (int, bool) {
	if !hasConditionals(r) {
		return 0, false
	}
	head, err := s.Objects.HeadObject(ctx, key)
	if err != nil {
		return 0, false
	}
	status, done := checkConditionals(r, head.ETag, head.LastModified)
	if !done {
		return 0, false
	}
	setValidatorHeaders(w, head.ETag, head.LastModified)
	w.WriteHeader(status)
	return status, true
}

// handleHead processes HEAD requests.
func (s *Server) handleHead(ctx context.Context, w http.ResponseWriter, r *http.Request, t *target) (int, error) {
	result, err := s.Objects.HeadObject(ctx, t.internalKey)
	if err != nil {
		return 0, err
	}
	annotateObject(ctx, result.Size, result.ContentType)

	setObjectHeaders(w, result.ContentType, result.Metadata, result.TagCount)
	w.Header().Set(httputil.HeaderContentLength, strconv.FormatInt(result.Size, 10))
	setValidatorHeaders(w, result.ETag, result.LastModified)

	if status, done := checkConditionals(r, result.ETag, result.LastModified); done {
		w.WriteHeader(status)
		return status, nil
	}

	w.WriteHeader(http.StatusOK)
	return http.StatusOK, nil
}

// handleDelete processes DELETE requests. The manager treats missing objects as
// success (S3 idempotent delete), so any error returned is a real backend failure.
func (s *Server) handleDelete(ctx context.Context, w http.ResponseWriter, t *target) (int, error) {
	if err := s.Objects.DeleteObject(ctx, t.internalKey); err != nil {
		return 0, err
	}

	w.WriteHeader(http.StatusNoContent)
	return http.StatusNoContent, nil
}

// resolveCopySource turns the x-amz-copy-source header into the internal key it
// names, for both CopyObject and UploadPartCopy. A source in another bucket is
// refused.
func resolveCopySource(r *http.Request, t *target) (string, error) {
	copySource := r.Header.Get(headerCopySource)
	decoded, err := url.PathUnescape(copySource)
	if err != nil {
		return "", fmt.Errorf("%w: %w", s3Err(http.StatusBadRequest, "InvalidArgument", "Invalid x-amz-copy-source encoding"), err)
	}
	source := strings.TrimPrefix(decoded, "/")
	sourceBucket, sourceKey, ok := parsePath("/" + source)
	if !ok || sourceKey == "" {
		return "", fmt.Errorf("%w: %q", s3Err(http.StatusBadRequest, "InvalidArgument", "Invalid x-amz-copy-source"), copySource)
	}
	if sourceBucket != t.bucket {
		return "", fmt.Errorf("%w: %s != %s", s3Err(http.StatusForbidden, s3CodeAccessDenied, "Cross-bucket copy is not allowed"),
			sourceBucket, t.bucket)
	}
	return internalkey.Make(t.bucket, sourceKey), nil
}

// handleCopyObject processes PUT requests with the x-amz-copy-source header.
// Copies an object from the source key to the destination key, potentially
// across backends, with atomic quota tracking. Only same-bucket copies are
// allowed (no cross-bucket copying).
func (s *Server) handleCopyObject(ctx context.Context, w http.ResponseWriter, r *http.Request, t *target) (int, error) {
	sourceInternalKey, err := resolveCopySource(r, t)
	if err != nil {
		return 0, err
	}

	// Refused before the copy so an unusable directive or tag set costs no
	// transfer, matching how the header is handled on a plain PUT.
	replaceTags, tags, err := parseTaggingDirective(r.Header)
	if err != nil {
		return 0, err
	}

	etag, err := s.Objects.CopyObject(ctx, &object.CopyObjectRequest{
		SourceKey:   sourceInternalKey,
		DestKey:     t.internalKey,
		ReplaceTags: replaceTags,
		Tags:        tags,
	})
	if err != nil {
		return 0, err
	}

	return respondCopy(w, "CopyObjectResult", etag, time.Now())
}

// handleDeleteObjects processes POST /{bucket}?delete requests. Deletes up to
// 1000 objects per request and returns per-key results in an XML response.
// Per the S3 spec, HTTP 200 is always returned even when individual keys fail.
func (s *Server) handleDeleteObjects(ctx context.Context, w http.ResponseWriter, r *http.Request, t *target) (int, error) {
	var req deleteObjectsRequest
	if err := decodeXMLBody(w, r, maxDeleteObjectsBody, &req); err != nil {
		return 0, err
	}

	if len(req.Objects) == 0 {
		return 0, s3Err(http.StatusBadRequest, "MalformedXML", "No objects specified for deletion")
	}
	if len(req.Objects) > 1000 {
		return 0, s3Err(http.StatusBadRequest, "MalformedXML", "DeleteObjects request may contain a maximum of 1000 objects")
	}

	keys := make([]string, len(req.Objects))
	for i, obj := range req.Objects {
		keys[i] = internalkey.Make(t.bucket, obj.Key)
	}

	results := s.Objects.DeleteObjects(ctx, keys)

	// A per-key failure answers with the same S3 error the whole request would
	// have, so a client reads one vocabulary.
	var resp deleteObjectsResult
	for i, res := range results {
		userKey := req.Objects[i].Key
		if res.Err != nil {
			s3e, _ := classifyError(res.Err)
			resp.Errors = append(resp.Errors, deleteObjectError{Key: userKey, Code: s3e.Code, Message: s3e.Message})
		} else if !req.Quiet {
			resp.Deleted = append(resp.Deleted, deletedObject{Key: userKey})
		}
	}

	return respondXML(w, resp)
}

// -------------------------------------------------------------------------
// CONDITIONAL REQUEST HELPERS
// -------------------------------------------------------------------------

// checkIfNoneMatchStar refuses a write carrying `If-None-Match: *` to a key
// that already holds an object, before the body upload starts. AWS S3 only
// honors the `*` form for write preconditions; specific-etag values are not
// interpreted on write.
func (s *Server) checkIfNoneMatchStar(ctx context.Context, r *http.Request, key string) error {
	if r.Header.Get(headerIfNoneMatch) != "*" {
		return nil
	}
	exists, err := s.Objects.ObjectExists(ctx, key)
	if err != nil {
		return err
	}
	if exists {
		return errIfNoneMatchExists
	}
	return nil
}

// setValidatorHeaders writes the ETag and Last-Modified that identify the
// representation. Split out because they must also appear on a 304, which
// carries no body and skips the rest of the header block.
func setValidatorHeaders(w http.ResponseWriter, etag string, lastModified time.Time) {
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	if !lastModified.IsZero() {
		w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))
	}
}

// hasConditionals reports whether the request carries a precondition that
// could be answered from the object's validators alone.
func hasConditionals(r *http.Request) bool {
	return r.Header.Get(headerIfMatch) != "" || r.Header.Get(headerIfNoneMatch) != "" ||
		r.Header.Get(headerIfModifiedSince) != "" || r.Header.Get(headerIfUnmodifiedSince) != ""
}

// setObjectHeaders writes the representation headers a GET and a HEAD of an
// object both carry: content type, range support, user metadata, and how many
// tags the object has, omitted for zero as S3 does.
func setObjectHeaders(w http.ResponseWriter, contentType string, metadata map[string]string, tagCount int) {
	w.Header().Set(httputil.HeaderContentType, contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	for k, v := range metadata {
		w.Header().Set("x-amz-meta-"+k, v)
	}
	if tagCount > 0 {
		w.Header().Set(headerTaggingCount, strconv.Itoa(tagCount))
	}
}

// ifRangeMatches reports whether an If-Range validator still describes the
// current representation; if not, the whole object is served instead of the
// range. An absent header matches. An HTTP date is compared against
// lastModified, and anything else as an entity tag using strong comparison
// (RFC 9110 13.1.5), so a weak tag never matches.
func ifRangeMatches(r *http.Request, etag string, lastModified time.Time) bool {
	ir := r.Header.Get(headerIfRange)
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
	if status, done := checkIfMatch(r.Header.Get(headerIfMatch), etag); done {
		return status, true
	}
	if status, done := checkIfUnmodifiedSince(r, lastModified); done {
		return status, true
	}
	inm := r.Header.Get(headerIfNoneMatch)
	if status, done := checkIfNoneMatch(inm, etag); done {
		return status, true
	}
	if status, done := checkIfModifiedSince(r.Header.Get(headerIfModifiedSince), inm, lastModified); done {
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
	ius := r.Header.Get(headerIfUnmodifiedSince)
	if ius == "" || r.Header.Get(headerIfMatch) != "" {
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

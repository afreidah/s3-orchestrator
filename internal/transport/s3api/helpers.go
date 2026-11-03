// -------------------------------------------------------------------------------
// Helpers - Request Guards, Path Parsing and XML Responses
//
// Author: Alex Freidah
//
// Utility functions for the server package. Guards return the S3 error a
// refusal answers with and never write; classifyError and fail turn every
// handler failure into its S3 XML response, and writeXML writes every document.
// -------------------------------------------------------------------------------

package s3api

import (
	"bytes"
	"cmp"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"
)

// xmlBufPool reuses bytes.Buffer instances for XML response encoding,
// avoiding a fresh allocation per response.
var xmlBufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// maxUserMetadataBytes is the S3-specified limit for total user metadata size.
const maxUserMetadataBytes = 2048

// s3XMLNS is the AWS S3 schema namespace echoed in every XML response
// envelope so v2 SDK clients can deserialize the result.
const s3XMLNS = "http://s3.amazonaws.com/doc/2006-03-01/"

// headerContentType is the canonical HTTP header name; using a single
// constant avoids string-literal duplication across handlers.
const headerContentType = "Content-Type"

// The conditional request headers a GET, HEAD, or PUT evaluates against an
// object's ETag and Last-Modified.
const (
	headerIfMatch           = "If-Match"
	headerIfNoneMatch       = "If-None-Match"
	headerIfModifiedSince   = "If-Modified-Since"
	headerIfUnmodifiedSince = "If-Unmodified-Since"
	headerIfRange           = "If-Range"
)

// headerTaggingCount reports how many tags an object carries on GET and HEAD.
// The wire name is "tagging-count" even though the SDKs expose the field as
// TagCount.
const headerTaggingCount = "x-amz-tagging-count"

// headerCopySource names the source object of a CopyObject or UploadPartCopy.
// Its presence is what distinguishes both from the PUT they share a route with.
const headerCopySource = "X-Amz-Copy-Source"

// headerCopySourceRange is the byte range of the source an UploadPartCopy
// takes as the part. Absent, the part is the whole source object.
const headerCopySourceRange = "x-amz-copy-source-range"

// -------------------------------------------------------------------------
// REQUEST GUARDS
// -------------------------------------------------------------------------

// checkSize refuses a body of size bytes over the configured object ceiling.
// what names the body in the message: an object, a part, or an assembly.
func (s *Server) checkSize(size int64, what string) error {
	if s.MaxObjectSize > 0 && size > s.MaxObjectSize {
		return fmt.Errorf("%w (%d bytes, limit %d)", s3Err(http.StatusRequestEntityTooLarge,
			"EntityTooLarge", what+" size exceeds the maximum allowed size"), size, s.MaxObjectSize)
	}
	return nil
}

// acceptBody refuses an upload body without a Content-Length or over the
// object ceiling, and caps what is read of one it accepts at that ceiling.
func (s *Server) acceptBody(w http.ResponseWriter, r *http.Request, what string) error {
	if r.ContentLength < 0 {
		return s3Err(http.StatusLengthRequired, "MissingContentLength", what+" Content-Length is required")
	}
	if err := s.checkSize(r.ContentLength, what); err != nil {
		return err
	}
	if s.MaxObjectSize > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, s.MaxObjectSize)
	}
	return nil
}

// uploadHeaders reads the content type and user metadata a new object carries,
// refusing metadata S3 would refuse.
func uploadHeaders(r *http.Request) (contentType string, meta map[string]string, err error) {
	meta = extractUserMetadata(r.Header)
	if err := validateUserMetadata(meta); err != nil {
		return "", nil, s3Err(http.StatusBadRequest, "MetadataTooLarge", err.Error())
	}
	return cmp.Or(r.Header.Get(headerContentType), "application/octet-stream"), meta, nil
}

// queryCount reads a non-negative count from the query, refusing a malformed
// one as S3 does with InvalidArgument.
func queryCount(t *target, param string, def, ceiling int) (int, error) {
	n, err := httputil.QueryCount(t.query, param, def, ceiling)
	if err != nil {
		return 0, s3Err(http.StatusBadRequest, "InvalidArgument", err.Error())
	}
	return n, nil
}

// -------------------------------------------------------------------------
// USER METADATA
// -------------------------------------------------------------------------

// extractUserMetadata extracts x-amz-meta-* headers from the request,
// lowercasing the metadata key names. Returns nil when no metadata is present.
func extractUserMetadata(h http.Header) map[string]string {
	var meta map[string]string
	for key, values := range h {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "x-amz-meta-") {
			name := lower[len("x-amz-meta-"):]
			if name != "" && len(values) > 0 {
				if meta == nil {
					meta = make(map[string]string)
				}
				meta[name] = values[0]
			}
		}
	}
	return meta
}

// findInvalidMetadataByte returns the first byte position outside the
// printable ASCII range and the offending byte. pos is -1 when s is
// fully valid. Used to build descriptive metadata validation errors
// that point operators directly at the bad byte instead of forcing
// them to scan the whole key or value.
func findInvalidMetadataByte(s string) (pos int, b byte) {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7E {
			return i, s[i]
		}
	}
	return -1, 0
}

// validateUserMetadata checks that metadata keys and values contain only
// safe characters (no CR/LF/control bytes) and that total size does not
// exceed the S3-specified 2 KB limit. Validation errors include the
// offending key, byte value, and position so operators can fix the
// offending request without inspecting raw bytes.
func validateUserMetadata(meta map[string]string) error {
	var total int
	for k, v := range meta {
		if pos, b := findInvalidMetadataByte(k); pos >= 0 {
			return fmt.Errorf("metadata key %q: invalid byte 0x%02x at position %d (only printable ASCII 0x20-0x7E allowed)", k, b, pos)
		}
		if pos, b := findInvalidMetadataByte(v); pos >= 0 {
			return fmt.Errorf("metadata key %q value: invalid byte 0x%02x at position %d (only printable ASCII 0x20-0x7E allowed)", k, b, pos)
		}
		total += len(k) + len(v)
	}
	if total > maxUserMetadataBytes {
		return fmt.Errorf("metadata size %d bytes exceeds limit %d (sum of all key+value lengths)", total, maxUserMetadataBytes)
	}
	return nil
}

// -------------------------------------------------------------------------
// CAPACITY HINT FORMATTING
// -------------------------------------------------------------------------

// formatCapacityHint renders a quota-stats snapshot as a comma-separated
// "name=used/limit" summary suitable for inclusion in the
// InsufficientStorage error body. Returns the empty string when stats
// is empty so the caller falls back to its terse default.
func formatCapacityHint(stats map[string]core.QuotaStat) string {
	if len(stats) == 0 {
		return ""
	}
	parts := make([]string, 0, len(stats))
	for name, s := range stats {
		parts = append(parts, fmt.Sprintf("%s=%s/%s", name,
			humanize.Bytes(max(0, s.BytesUsed)), humanize.Bytes(max(0, s.BytesLimit))))
	}
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}

// -------------------------------------------------------------------------
// PATH AND QUERY PARSING
// -------------------------------------------------------------------------

// BucketFromPath reports the virtual bucket a request path addresses, for
// middleware that runs before routing.
func BucketFromPath(path string) (string, bool) {
	bucket, _, ok := parsePath(path)
	return bucket, ok
}

// parsePath extracts bucket and key from the URL path.
// Expected format: /{bucket} or /{bucket}/{key...}
// When no key is present, key is empty (used for bucket-level operations like
// ListObjectsV2).
func parsePath(path string) (bucket string, key string, ok bool) {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "", "", false
	}
	parts := strings.SplitN(path, "/", 2)
	if parts[0] == "" {
		return "", "", false
	}
	if len(parts) == 1 || parts[1] == "" {
		return parts[0], "", true
	}
	return parts[0], parts[1], true
}

// Query keys an object request may carry. S3 selects the operation from the
// query string, not just the method, so a key absent from this set names an
// operation this server does not implement. It is an allow-list so that an
// unknown subresource is rejected instead of falling through to the data path,
// where it could overwrite or delete an object.
var supportedObjectQueryKeys = map[string]bool{
	"uploads":            true, // CreateMultipartUpload / ListMultipartUploads
	"uploadId":           true, // per-upload multipart operations
	"partNumber":         true, // UploadPart
	"max-parts":          true, // ListParts paging
	"part-number-marker": true,
	"tagging":            true, // Put/Get/DeleteObjectTagging

	// x-id names the SDK operation that built the request (x-id=PutObject).
	// Added by the AWS SDKs, carries no meaning for the server, and appears on
	// ordinary data-path calls, so refusing it rejects normal traffic.
	"x-id": true,
}

// Query key prefixes an object request may carry. X-Amz- covers presigned URL
// credentials, and response- covers S3's response-header overrides such as
// response-content-disposition.
var supportedObjectQueryPrefixes = []string{"X-Amz-", "response-"}

// Query keys a bucket request may carry: the four subresources this server
// serves, the parameters its two listings and its upload listing read, and
// x-id. Without the allow-list an unsupported subresource such as versions
// would be answered by ListObjects, which a client would read as an empty
// result instead of an unsupported operation.
var supportedBucketQueryKeys = map[string]bool{
	"delete":             true, // DeleteObjects
	"location":           true, // GetBucketLocation
	"uploads":            true, // ListMultipartUploads
	"versioning":         true, // GetBucketVersioning
	"list-type":          true, // selects ListObjectsV2 over V1
	"prefix":             true,
	"delimiter":          true,
	"marker":             true,
	"max-keys":           true,
	"encoding-type":      true,
	"continuation-token": true,
	"start-after":        true,
	"fetch-owner":        true,
	"key-marker":         true, // ListMultipartUploads paging
	"upload-id-marker":   true,
	"max-uploads":        true,
	"x-id":               true, // SDK-supplied operation name, meaningless here
}

// Query key prefixes a bucket request may carry. A presigned listing puts its
// credentials in the query string, so refusing the X-Amz- family would break
// every presigned ListObjects. The response- overrides are object-only.
var supportedBucketQueryPrefixes = []string{"X-Amz-"}

// unsupportedQuery returns the first query key outside allowed, so a router can
// refuse before dispatch rather than fall through to the data path. prefixes
// are matched as prefixes, which is what lets a whole parameter family pass
// without naming each member. Returns ok=false when every key is recognised.
func unsupportedQuery(query url.Values, allowed map[string]bool, prefixes []string) (string, bool) {
	for key := range query {
		if allowed[key] {
			continue
		}
		if slices.ContainsFunc(prefixes, func(p string) bool {
			return strings.HasPrefix(key, p)
		}) {
			continue
		}
		return key, true
	}
	return "", false
}

// -------------------------------------------------------------------------
// ERROR RESPONSES
// -------------------------------------------------------------------------

// s3CodeAccessDenied is the S3 error code a refused request carries.
const s3CodeAccessDenied = "AccessDenied"

// errInternal is the answer to a failure no S3 error describes. The client is
// never shown the internal message; the request log keeps it.
var errInternal = s3Err(http.StatusBadGateway, "InternalError", "We encountered an internal error. Please try again.")

// s3Err builds the S3 error a refusal answers with. A guard wraps it with the
// specifics the request log keeps, which the client does not see.
func s3Err(status int, code, message string) *core.S3Error {
	return &core.S3Error{StatusCode: status, Code: code, Message: message}
}

// classifyError names the S3 error a failure answers with, and the streaming
// rejection reason the metrics count it under when it is one. Tag validation
// keeps its own message, which names the offending value. Anything else not
// carrying an S3 error is errInternal.
func classifyError(err error) (*core.S3Error, string) {
	if fault, ok := asStreamingFault(err); ok {
		return fault.s3, fault.reason
	}
	switch {
	case errors.Is(err, core.ErrTooManyTags):
		return s3Err(http.StatusBadRequest, "BadRequest", err.Error()), ""
	case errors.Is(err, errInvalidTaggingDirective):
		return s3Err(http.StatusBadRequest, "InvalidArgument", err.Error()), ""
	case core.IsTagValidationError(err):
		return s3Err(http.StatusBadRequest, "InvalidTag", err.Error()), ""
	}
	if s3e, ok := errors.AsType[*core.S3Error](err); ok {
		return s3e, ""
	}
	return errInternal, ""
}

// fail writes err as the S3 error it classifies as and returns that status.
// Every handler failure reaches the client through here.
func fail(w http.ResponseWriter, err error) int {
	s3e, reason := classifyError(err)
	if reason != "" {
		telemetry.AuthStreamingRejectionsTotal.WithLabelValues(reason).Inc()
	}
	WriteS3Error(w, s3e.StatusCode, s3e.Code, s3e.Message)
	return s3e.StatusCode
}

// errorDocument is the S3 XML error body.
type errorDocument struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

// WriteS3Error writes an S3 XML error. It matches httputil.ErrorWriter, so the
// middleware in front of the S3 API refuses in the format its clients read.
func WriteS3Error(w http.ResponseWriter, status int, code, message string) {
	_, _ = writeXML(w, status, errorDocument{Code: code, Message: message})
}

// -------------------------------------------------------------------------
// XML DOCUMENTS
// -------------------------------------------------------------------------

// s3Namespace is the xmlns attribute of every S3 response document. It always
// marshals as the S3 schema namespace, so a document declaring the field needs
// nothing set; v2 SDK clients do not deserialize a document without it.
type s3Namespace struct{}

// MarshalXMLAttr writes the S3 schema namespace.
func (s3Namespace) MarshalXMLAttr(xml.Name) (xml.Attr, error) {
	return xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: s3XMLNS}, nil
}

// UnmarshalXMLAttr accepts the namespace when a document is read back. It
// carries nothing to keep.
func (*s3Namespace) UnmarshalXMLAttr(xml.Attr) error {
	return nil
}

// Request-body ceilings for the two XML control-plane operations. Both sit
// above the largest request the S3 API permits, so a legal client is never
// rejected: a 1000-object delete with maximum-length keys is roughly 1.05 MB,
// and a 10000-part manifest roughly 900 KB before SDK indentation.
const (
	maxDeleteObjectsBody     = 4 << 20
	maxCompleteMultipartBody = 2 << 20
)

// decodeXMLBody reads exactly one XML document from the request body into v.
// Trailing content after the document is refused, so the server never acts on
// something other than what an upstream inspector saw.
//
// It uses MaxBytesReader rather than io.LimitReader, which would silently
// truncate an oversized body and could accept a prefix that happens to be a
// complete document.
func decodeXMLBody(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	dec := xml.NewDecoder(http.MaxBytesReader(w, r.Body, limit))

	if err := dec.Decode(v); err != nil {
		return xmlBodyError(err)
	}

	// Walk what remains rather than decoding again: a second Decode skips
	// character data looking for a start element, so bare junk after the
	// document reads as a clean EOF. Trailing whitespace is tolerated because
	// SDKs routinely append a newline; anything else is refused.
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return xmlBodyError(err)
		}
		text, isText := tok.(xml.CharData)
		if !isText || len(bytes.TrimSpace(text)) > 0 {
			return s3Err(http.StatusBadRequest, "MalformedXML", "Request body must contain exactly one XML document")
		}
	}
}

// xmlBodyError separates a request body that ran past its ceiling from one
// that is simply not valid XML. A LimitReader could not tell those apart, so an
// oversized request was reported as malformed.
func xmlBodyError(err error) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return fmt.Errorf("%w: %w", s3Err(http.StatusRequestEntityTooLarge, "MaxMessageLengthExceeded",
			"Request body exceeds the maximum allowed size"), err)
	}
	return fmt.Errorf("%w: %w", s3Err(http.StatusBadRequest, "MalformedXML", "Failed to parse request body"), err)
}

// writeXML encodes v as a document and writes it with status. The document is
// encoded to a pooled buffer before anything is sent, so an encoding failure
// leaves the response unwritten and reports status zero for the caller to fail.
func writeXML(w http.ResponseWriter, status int, v any) (int, error) {
	buf := xmlBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer xmlBufPool.Put(buf)

	buf.WriteString(xml.Header)
	if err := xml.NewEncoder(buf).Encode(v); err != nil {
		return 0, fmt.Errorf("encode XML response: %w", err)
	}
	w.Header().Set(headerContentType, "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	if _, err := w.Write(buf.Bytes()); err != nil {
		return status, fmt.Errorf("write XML response: %w", err)
	}
	return status, nil
}

// respondXML writes v as a 200 XML response.
func respondXML(w http.ResponseWriter, v any) (int, error) {
	return writeXML(w, http.StatusOK, v)
}

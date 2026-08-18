// -------------------------------------------------------------------------------
// Helpers - Path Parsing and Error Formatting
//
// Author: Alex Freidah
//
// Utility functions for the server package. Handles URL path parsing for S3-style
// bucket/key extraction and S3-compatible XML error response formatting.
// -------------------------------------------------------------------------------

package s3api

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
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

// -------------------------------------------------------------------------
// REQUEST GUARDS
// -------------------------------------------------------------------------

// enforceContentLength applies the standard guard for any S3 PUT-style
// handler: reject missing Content-Length with 411, reject oversized payloads
// with 413, and otherwise wrap the body in MaxBytesReader. The label is
// folded into the user-facing error message so callers retain their distinct
// vocabulary ("Object" vs "Part" etc.). Returns (statusCode, error, ok). When
// ok is false the caller must propagate the (status, error) without further
// processing  -  the response has already been written.
func enforceContentLength(w http.ResponseWriter, r *http.Request, maxSize int64, label string) (int, error, bool) {
	if r.ContentLength < 0 {
		writeS3Error(w, http.StatusLengthRequired, "MissingContentLength", label+" Content-Length is required")
		return http.StatusLengthRequired, fmt.Errorf("missing Content-Length"), false
	}
	if maxSize > 0 && r.ContentLength > maxSize {
		writeS3Error(w, http.StatusRequestEntityTooLarge, "EntityTooLarge", label+" size exceeds the maximum allowed size")
		return http.StatusRequestEntityTooLarge, fmt.Errorf("%s size %d exceeds max %d", label, r.ContentLength, maxSize), false
	}
	if maxSize > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxSize)
	}
	return 0, nil, true
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

// parseQueryInt parses an integer query parameter, clamping it to [1, max].
// Returns defaultVal when the parameter is absent or invalid.
func parseQueryInt(r *http.Request, param string, defaultVal, max int) int {
	s := r.URL.Query().Get(param)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 || v > max {
		return defaultVal
	}
	return v
}

// -------------------------------------------------------------------------
// RESPONSE HELPERS
// -------------------------------------------------------------------------

// writeS3Error sends an S3-style XML error response with Content-Length.
func writeS3Error(w http.ResponseWriter, code int, errCode, message string) {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Error>
  <Code>%s</Code>
  <Message>%s</Message>
</Error>`, xmlEscape(errCode), xmlEscape(message))
	w.Header().Set(headerContentType, "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body) //nolint:gosec // G705: output is XML-escaped via xmlEscape before writing
}

// WriteS3Error is the exported form of writeS3Error so other transport
// packages (notably the panic-recovery middleware in httputil) can emit
// a route-appropriate S3-XML 500 without re-implementing the envelope.
// Matches the httputil.ErrorWriter signature exactly so it slots in as
// a direct argument.
func WriteS3Error(w http.ResponseWriter, code int, errCode, message string) {
	writeS3Error(w, code, errCode, message)
}

// xmlReplacer escapes special XML characters. Allocated once at package level
// to avoid per-call allocation.
var xmlReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

// xmlEscape escapes special XML characters.
func xmlEscape(s string) string {
	return xmlReplacer.Replace(s)
}

// writeStorageError checks if err is an *storage.S3Error and writes the
// appropriate S3 XML error response. Streaming-validation failures take
// precedence so a tampered chunk surfaces as 403 SignatureDoesNotMatch
// rather than the generic InternalError fallback. Returns the HTTP
// status code used.
func writeStorageError(w http.ResponseWriter, err error, fallbackMsg string) int {
	if status, code, msg, reason, ok := streamingErrorResponse(err); ok {
		telemetry.AuthStreamingRejectionsTotal.WithLabelValues(reason).Inc()
		writeS3Error(w, status, code, msg)
		return status
	}
	if s3err, ok := errors.AsType[*core.S3Error](err); ok {
		writeS3Error(w, s3err.StatusCode, s3err.Code, s3err.Message)
		return s3err.StatusCode
	}
	writeS3Error(w, http.StatusBadGateway, "InternalError", fallbackMsg)
	return http.StatusBadGateway
}

// Request-body ceilings for the two XML control-plane operations. Both sit
// above the largest request the S3 API permits, so a legal client is never
// rejected: a 1000-object delete with maximum-length keys is roughly 1.05 MB,
// and a 10000-part manifest roughly 900 KB before SDK indentation.
const (
	maxDeleteObjectsBody     = 4 << 20
	maxCompleteMultipartBody = 2 << 20
)

// decodeXMLBody reads exactly one XML document from the request body into v,
// writes the matching S3 error response on failure, and returns the status it
// used.
//
// MaxBytesReader rather than io.LimitReader: a LimitReader silently truncates
// at the ceiling, which reports an oversized body as malformed XML and, worse,
// accepts it outright whenever the prefix happens to be a complete document.
// MaxBytesReader fails instead, and lets the server close the connection
// rather than leaving unread bytes in the pipe.
//
// The second decode is what rejects trailing content. Decode stops at the end
// of the first document, so without this a second document or any junk after
// the first is accepted and silently discarded - and what the orchestrator
// then acts on is not what anything upstream inspected.
func decodeXMLBody(w http.ResponseWriter, r *http.Request, limit int64, v any) (int, error) {
	dec := xml.NewDecoder(http.MaxBytesReader(w, r.Body, limit))

	if err := dec.Decode(v); err != nil {
		return xmlBodyError(w, err)
	}

	// Walk what remains rather than decoding again: a second Decode skips
	// character data looking for a start element, so bare junk after the
	// document reads as a clean EOF. Trailing whitespace is tolerated because
	// SDKs routinely append a newline; anything else is refused.
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return http.StatusOK, nil
		}
		if err != nil {
			return xmlBodyError(w, err)
		}
		text, isText := tok.(xml.CharData)
		if !isText || len(bytes.TrimSpace(text)) > 0 {
			writeS3Error(w, http.StatusBadRequest, "MalformedXML",
				"Request body must contain exactly one XML document")
			return http.StatusBadRequest, errors.New("trailing content after XML document")
		}
	}
}

// xmlBodyError renders a request-body failure, separating a body that ran past
// its ceiling from one that is simply not valid XML. A LimitReader could not
// tell those apart, so an oversized request was reported as malformed.
func xmlBodyError(w http.ResponseWriter, err error) (int, error) {
	if maxErr, ok := errors.AsType[*http.MaxBytesError](err); ok {
		writeS3Error(w, http.StatusRequestEntityTooLarge, "MaxMessageLengthExceeded",
			"Request body exceeds the maximum allowed size")
		return http.StatusRequestEntityTooLarge, fmt.Errorf("request body over %d bytes: %w", maxErr.Limit, err)
	}
	writeS3Error(w, http.StatusBadRequest, "MalformedXML", "Failed to parse request body")
	return http.StatusBadRequest, fmt.Errorf("decode request body: %w", err)
}

// writeXML writes an S3-compatible XML response with the standard XML header.
// Encodes to a pooled buffer first so a serialization error doesn't commit a
// success status to the client.
func writeXML(w http.ResponseWriter, status int, v any) error {
	buf := xmlBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer xmlBufPool.Put(buf)

	buf.WriteString(xml.Header)
	if err := xml.NewEncoder(buf).Encode(v); err != nil {
		return err
	}
	w.Header().Set(headerContentType, "application/xml")
	w.WriteHeader(status)
	_, err := w.Write(buf.Bytes())
	return err
}

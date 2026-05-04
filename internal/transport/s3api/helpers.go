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
	"strconv"
	"strings"
	"sync"

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

// validMetadataToken reports whether s contains only printable ASCII
// characters suitable for use in an HTTP header field (no CR, LF, or
// other control characters). This prevents HTTP header injection via
// user-supplied metadata keys and values.
func validMetadataToken(s string) bool {
	for _, c := range s {
		if c < 0x20 || c > 0x7E {
			return false
		}
	}
	return true
}

// validateUserMetadata checks that metadata keys and values contain only
// safe characters (no CR/LF/control bytes) and that total size does not
// exceed the S3-specified 2 KB limit.
func validateUserMetadata(meta map[string]string) error {
	var total int
	for k, v := range meta {
		if !validMetadataToken(k) || !validMetadataToken(v) {
			return fmt.Errorf("metadata contains invalid characters")
		}
		total += len(k) + len(v)
	}
	if total > maxUserMetadataBytes {
		return fmt.Errorf("metadata size %d exceeds limit %d", total, maxUserMetadataBytes)
	}
	return nil
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
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body) //nolint:gosec // G705: output is XML-escaped via xmlEscape before writing
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
// appropriate S3 XML error response. Falls back to 502 InternalError for
// untyped errors. Returns the HTTP status code used.
func writeStorageError(w http.ResponseWriter, err error, fallbackMsg string) int {
	var s3err *core.S3Error
	if errors.As(err, &s3err) {
		writeS3Error(w, s3err.StatusCode, s3err.Code, s3err.Message)
		return s3err.StatusCode
	}
	writeS3Error(w, http.StatusBadGateway, "InternalError", fallbackMsg)
	return http.StatusBadGateway
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
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, err := w.Write(buf.Bytes())
	return err
}

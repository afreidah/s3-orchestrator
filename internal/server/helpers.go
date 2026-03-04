// -------------------------------------------------------------------------------
// Helpers - Path Parsing and Error Formatting
//
// Author: Alex Freidah
//
// Utility functions for the server package. Handles URL path parsing for S3-style
// bucket/key extraction and S3-compatible XML error response formatting.
// -------------------------------------------------------------------------------

package server

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/storage"
)

// maxUserMetadataBytes is the S3-specified limit for total user metadata size.
const maxUserMetadataBytes = 2048

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

// validateUserMetadata checks that total user metadata does not exceed
// the S3-specified 2 KB limit.
func validateUserMetadata(meta map[string]string) error {
	var total int
	for k, v := range meta {
		total += len(k) + len(v)
	}
	if total > maxUserMetadataBytes {
		return fmt.Errorf("metadata size %d exceeds limit %d", total, maxUserMetadataBytes)
	}
	return nil
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

// writeS3Error sends an S3-style XML error response.
func writeS3Error(w http.ResponseWriter, code int, errCode, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error>
  <Code>%s</Code>
  <Message>%s</Message>
</Error>`, xmlEscape(errCode), xmlEscape(message))
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
	var s3err *storage.S3Error
	if errors.As(err, &s3err) {
		writeS3Error(w, s3err.StatusCode, s3err.Code, s3err.Message)
		return s3err.StatusCode
	}
	writeS3Error(w, http.StatusBadGateway, "InternalError", fallbackMsg)
	return http.StatusBadGateway
}

// writeXML writes an S3-compatible XML response with the standard XML header.
// Encodes to a buffer first so a serialization error doesn't commit a success
// status to the client.
func writeXML(w http.ResponseWriter, status int, v any) error {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	if err := xml.NewEncoder(&buf).Encode(v); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, err := w.Write(buf.Bytes())
	return err
}

// -------------------------------------------------------------------------------
// HTTP JSON Request/Response Helpers
//
// Author: Alex Freidah
//
// Shared helpers every transport handler uses to read and write JSON.
// Centralising them ensures error responses are always encoded JSON, so a
// message containing a quote or backslash cannot corrupt the body, enforces
// a request body size cap on every decode path, and keeps method-validation
// boilerplate out of individual handlers.
// -------------------------------------------------------------------------------

package httputil

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
)

// The Err values are the user-facing messages, written when DecodeJSONBody
// cannot parse a request body and when RequireMethod rejects a method.
// MaxJSONBody is the read cap for a request body that carries only a few
// fields.
const (
	contentTypeJSON   = "application/json"
	headerContentType = "Content-Type"
	MediaOctetStream  = "application/octet-stream"
	MaxJSONBody       = 1 << 20

	ErrInvalidRequestBody = "invalid request body"
	ErrMethodNotAllowed   = "method not allowed"
)

// WriteJSON writes body as a JSON response with the given status. Encode errors
// are dropped because the header is already committed.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set(headerContentType, contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteJSONError writes a JSON error response with the correct content
// type and status. Encodes the payload through encoding/json so quote or
// backslash characters in msg cannot corrupt the response body.
func WriteJSONError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// InternalError logs err under msg with attrs and answers 500 with msg, so the
// client sees the operator-facing message and the log keeps the cause.
func InternalError(ctx context.Context, log *slog.Logger, w http.ResponseWriter, msg string, err error, attrs ...any) {
	log.ErrorContext(ctx, msg, append([]any{logfmt.Err(err)}, attrs...)...)
	WriteJSONError(w, http.StatusInternalServerError, msg)
}

// WriteObjectError renders a failed object operation. A rejected request names
// what was wrong with it, a missing object is a 404, and anything else is a
// server fault logged under msg.
func WriteObjectError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error, msg, key string) {
	switch {
	case errors.Is(err, ops.ErrKeyRequired),
		errors.Is(err, ops.ErrPrefixRequired),
		errors.Is(err, ops.ErrInvalidKey):
		WriteJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ops.ErrNotFound):
		WriteJSONError(w, http.StatusNotFound, "not found")
	default:
		InternalError(r.Context(), log, w, msg, err, slog.String("key", key))
	}
}

// ServeAttachment streams body as a file download named after the last path
// element of name. A size of zero or less leaves Content-Length off. The
// error is the copy's, for a caller that logs an interrupted download.
func ServeAttachment(w http.ResponseWriter, name string, size int64, body io.Reader) error {
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(path.Base(name)))
	w.Header().Set(headerContentType, MediaOctetStream)
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	_, err := bufpool.Copy(w, body)
	return err
}

// DecodeJSONBody applies a maxBytes read cap to the request body and
// decodes it into dst. Returns true on success; on failure it writes a
// 400 JSON error response and returns false so the caller can return
// early without duplicating the response boilerplate.
func DecodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		WriteJSONError(w, http.StatusBadRequest, ErrInvalidRequestBody)
		return false
	}
	return true
}

// RequireMethod verifies r.Method matches one of the allowed methods.
// Returns true when the request may proceed; on mismatch it sets the
// Allow header, writes a 405 JSON error, and returns false so the
// caller can return early.
func RequireMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	if slices.Contains(methods, r.Method) {
		return true
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	WriteJSONError(w, http.StatusMethodNotAllowed, ErrMethodNotAllowed)
	return false
}

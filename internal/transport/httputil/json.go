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

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
)

// The Header and Media values are the header names and media types every
// surface writes. The Err values are the user-facing messages, written when
// DecodeJSONBody cannot parse a request body and when RequireMethod rejects a
// method. MaxJSONBody is the read cap for a request body that carries only a
// few fields.
const (
	HeaderContentType   = "Content-Type"
	HeaderContentLength = "Content-Length"
	MediaJSON           = "application/json"
	MediaXML            = "application/xml"
	MediaOctetStream    = "application/octet-stream"
	MaxJSONBody         = 1 << 20

	ErrInvalidRequestBody = "invalid request body"
	ErrMethodNotAllowed   = "method not allowed"
)

// WriteJSON writes body as a JSON response with the given status. Encode errors
// are dropped because the header is already committed.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set(HeaderContentType, MediaJSON)
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
	log.ErrorContext(ctx, msg, append([]any{"error", err}, attrs...)...)
	WriteJSONError(w, http.StatusInternalServerError, msg)
}

// WriteOpError renders a failed operation. A rejection tells the caller what
// was wrong with the request under its kind's status; anything else is a
// server fault logged under msg with attrs.
func WriteOpError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error, msg string, attrs ...any) {
	if rej, ok := errors.AsType[*ops.Rejection](err); ok {
		WriteJSONError(w, rejectionStatus(rej.Kind), err.Error())
		return
	}
	InternalError(r.Context(), log, w, msg, err, attrs...)
}

// rejectionStatus is the status a rejection of kind answers with.
func rejectionStatus(kind ops.RejectionKind) int {
	switch kind {
	case ops.RejectNotFound:
		return http.StatusNotFound
	case ops.RejectForbidden:
		return http.StatusForbidden
	case ops.RejectConflict:
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// ServeAttachment streams body as a file download named after the last path
// element of name. A size of zero or less leaves Content-Length off. The
// error is the copy's, for a caller that logs an interrupted download.
func ServeAttachment(w http.ResponseWriter, name string, size int64, body io.Reader) error {
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(path.Base(name)))
	w.Header().Set(HeaderContentType, MediaOctetStream)
	if size > 0 {
		w.Header().Set(HeaderContentLength, strconv.FormatInt(size, 10))
	}
	_, err := bufpool.Copy(w, body)
	return err
}

// DecodeJSONBody applies a maxBytes read cap to the request body and decodes
// exactly one JSON value into dst, refusing anything but whitespace after it.
// On failure it writes a 400 JSON error and returns false so the caller can
// return early.
func DecodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil || !errors.Is(dec.Decode(&struct{}{}), io.EOF) {
		WriteJSONError(w, http.StatusBadRequest, ErrInvalidRequestBody)
		return false
	}
	return true
}

// AllowMethod wraps next so it serves only method, answering any other with a
// 405 naming it. A GET route serves HEAD too, as a method-scoped mux pattern
// does.
func AllowMethod(method string, next http.HandlerFunc) http.HandlerFunc {
	allowed := []string{method}
	if method == http.MethodGet {
		allowed = append(allowed, http.MethodHead)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if RequireMethod(w, r, allowed...) {
			next(w, r)
		}
	}
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

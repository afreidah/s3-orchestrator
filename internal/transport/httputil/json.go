// -------------------------------------------------------------------------------
// HTTP JSON Request/Response Helpers
//
// Author: Alex Freidah
//
// Shared helpers every transport handler uses to read and write JSON.
// Centralising them ensures error responses never escape with malformed
// JSON (a previous string-concatenated implementation in the UI package
// would corrupt the body when the message contained a quote or
// backslash), enforces a request body size cap on every decode path,
// and keeps method-validation boilerplate out of individual handlers.
// -------------------------------------------------------------------------------

package httputil

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// The Err values are the user-facing messages, written when DecodeJSONBody
// cannot parse a request body and when RequireMethod rejects a method.
const (
	contentTypeJSON   = "application/json"
	headerContentType = "Content-Type"

	ErrInvalidRequestBody = "invalid request body"
	ErrMethodNotAllowed   = "method not allowed"
)

// WriteJSON serialises body as JSON, sets the JSON content-type header,
// writes the status code, and emits the encoded payload. Encoder write
// errors are swallowed because the response is already committed by
// WriteHeader; logging is the caller's responsibility when it matters.
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

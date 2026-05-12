// -------------------------------------------------------------------------------
// UI Handler - JSON Request/Response Helpers
//
// Author: Alex Freidah
//
// Shared helpers every JSON-handling endpoint in this package uses. Keeping
// them centralised means an error response can never escape with malformed
// JSON (the previous string-concatenated writeJSONError implementation
// would corrupt the body when the message contained a quote or backslash),
// and the header/encode pair only needs to be audited in one place.
// -------------------------------------------------------------------------------

package ui

import (
	"encoding/json"
	"net/http"
)

// writeJSON serialises body as JSON, sets the JSON content-type header,
// writes the status code, and emits the encoded payload. Encoder write
// errors are swallowed because the response is already committed by
// WriteHeader; logging is the caller's responsibility when it matters.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set(headerContentType, contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeJSONError writes a JSON error response with the correct content
// type and status. Encodes the payload through encoding/json so quote or
// backslash characters in msg cannot corrupt the response body.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeJSON applies a 1 MiB read cap to the request body and decodes it
// into dst. Returns true on success; on failure it writes a 400 JSON
// error response describing the parse failure and returns false so the
// caller can return early without duplicating the response boilerplate.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeJSONError(w, http.StatusBadRequest, errInvalidRequestBody)
		return false
	}
	return true
}

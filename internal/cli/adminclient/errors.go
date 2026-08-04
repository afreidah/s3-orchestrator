// -------------------------------------------------------------------------------
// Admin API Client - Typed Errors
//
// Author: Alex Freidah
//
// A non-2xx response carries information beyond "it failed": the status
// separates a broken endpoint from one the deployment simply does not offer,
// and the body carries a message written for a human. Both consumers used to
// extract that message with their own parser, written months apart against the
// same two body shapes.
// -------------------------------------------------------------------------------

package adminclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Error is a non-2xx admin API response.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string {
	if d := e.Detail(); d != "" {
		return fmt.Sprintf("admin API returned %d: %s", e.Status, d)
	}
	return fmt.Sprintf("admin API returned %d", e.Status)
}

// Unavailable reports whether the endpoint is absent by configuration rather
// than broken: /admin/api/workers answers 503 on a proxy-only instance, and
// every /admin/api/cache route answers 503 when object caching is off.
func (e *Error) Unavailable() bool { return e.Status == http.StatusServiceUnavailable }

// Detail extracts the human-readable part of an error body, which the admin
// API writes as either {"error":...} or, for a disabled subsystem,
// {"status":"disabled","reason":...}. Falls back to the raw body so an
// unrecognised shape is still legible.
func (e *Error) Detail() string {
	var parsed struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(e.Body), &parsed) == nil {
		if parsed.Error != "" {
			return parsed.Error
		}
		if parsed.Reason != "" {
			return parsed.Reason
		}
	}
	return e.Body
}

// UnavailableReason returns the explanation for an endpoint the deployment
// does not offer, or "" when err is any other kind of failure. Callers use it
// to report configuration as configuration rather than as a failure.
func UnavailableReason(err error) string {
	apiErr, ok := errors.AsType[*Error](err)
	if !ok || !apiErr.Unavailable() {
		return ""
	}
	if d := apiErr.Detail(); d != "" {
		return d
	}
	return "not available on this instance"
}

// readError drains an error response into an *Error.
func readError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return &Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
}

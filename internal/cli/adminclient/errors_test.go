// -------------------------------------------------------------------------------
// Admin API Client - Error Tests
//
// Author: Alex Freidah
//
// Pins the two error-body shapes the admin API emits and the rule that only a
// 503 counts as "this deployment does not offer the endpoint" rather than a
// failure. Both consumers previously carried their own copy of this parsing.
// -------------------------------------------------------------------------------

package adminclient

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestError_UnavailableReason covers the two error-body shapes the admin API
// emits, and asserts only a 503 is treated as a configuration fact: any other
// status is a genuine failure the pane must surface as an error.
func TestError_UnavailableReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"disabled subsystem", &Error{Status: http.StatusServiceUnavailable, Body: `{"status":"disabled","reason":"caching is off"}`}, "caching is off"},
		{"error body", &Error{Status: http.StatusServiceUnavailable, Body: `{"error":"not wired"}`}, "not wired"},
		{"empty body", &Error{Status: http.StatusServiceUnavailable}, "not available on this instance"},
		{"unparseable body", &Error{Status: http.StatusServiceUnavailable, Body: "plain text"}, "plain text"},
		{"other status", &Error{Status: http.StatusInternalServerError, Body: `{"error":"boom"}`}, ""},
		{"not an Error", errors.New("boom"), ""},
	}
	for _, c := range cases {
		if got := UnavailableReason(c.err); got != c.want {
			t.Errorf("%s: unavailableReason = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestError_Error asserts the message carries the status and the body's
// human-readable part rather than raw JSON.
func TestError_Error(t *testing.T) {
	t.Parallel()
	err := &Error{Status: http.StatusForbidden, Body: `{"error":"denied"}`}
	if got := err.Error(); !strings.Contains(got, "403") || !strings.Contains(got, "denied") {
		t.Errorf("Error() = %q", got)
	}
	bare := &Error{Status: http.StatusBadGateway}
	if got := bare.Error(); !strings.Contains(got, "502") {
		t.Errorf("bare Error() = %q", got)
	}
}

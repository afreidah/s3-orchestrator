// -------------------------------------------------------------------------------
// Query Integer Fuzz Tests - Query Parameter Parsing
//
// Author: Alex Freidah
//
// Fuzz tests for parseQueryInt covering malformed, adversarial, and edge-case
// query parameter values. Validates clamping invariants on output.
// -------------------------------------------------------------------------------

package s3api

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// FuzzParseQueryInt exercises the query parameter integer parser with
// arbitrary string values. Asserts that the result is always within the
// valid [1, max] range or equals the default.
func FuzzParseQueryInt(f *testing.F) {
	f.Add("100", 1000, 1000)
	f.Add("0", 1000, 1000)
	f.Add("-1", 1000, 1000)
	f.Add("", 1000, 1000)
	f.Add("abc", 1000, 1000)
	f.Add("1", 10, 100)
	f.Add("999999999999", 1000, 1000)
	f.Add("1", 1, 1)

	f.Fuzz(func(t *testing.T, value string, defaultVal, maxVal int) {
		if maxVal < 1 || defaultVal < 1 {
			return
		}

		r := &http.Request{URL: &url.URL{RawQuery: fmt.Sprintf("p=%s", url.QueryEscape(value))}}
		result := parseQueryInt(r, "p", defaultVal, maxVal)

		if result < 1 {
			t.Errorf("parseQueryInt(%q, default=%d, max=%d) = %d, want >= 1", value, defaultVal, maxVal, result)
		}
		if result > maxVal && result != defaultVal {
			t.Errorf("parseQueryInt(%q, default=%d, max=%d) = %d, exceeds max without being default", value, defaultVal, maxVal, result)
		}
	})
}

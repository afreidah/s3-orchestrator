// -------------------------------------------------------------------------------
// Listing Count Fuzz Tests - Paging Parameter Parsing
//
// Author: Alex Freidah
//
// Fuzz tests for parseListingCount covering malformed, adversarial, and
// edge-case paging values. Validates the default, ceiling, and refusal
// invariants on output.
// -------------------------------------------------------------------------------

package s3api

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// FuzzParseListingCount exercises the paging parameter parser with arbitrary
// string values. An absent value yields the default, an accepted one lies in
// [0, ceiling], and only a present value can be refused.
func FuzzParseListingCount(f *testing.F) {
	f.Add("100", 1000, 1000)
	f.Add("0", 1000, 1000)
	f.Add("-1", 1000, 1000)
	f.Add("", 1000, 1000)
	f.Add("abc", 1000, 1000)
	f.Add("1", 10, 100)
	f.Add("999999999999", 1000, 1000)
	f.Add("2147483647", 1000, 1000)
	f.Add("1", 1, 1)

	f.Fuzz(func(t *testing.T, value string, defaultVal, ceiling int) {
		if ceiling < 0 || defaultVal < 0 || defaultVal > ceiling {
			return
		}

		r := &http.Request{URL: &url.URL{RawQuery: fmt.Sprintf("p=%s", url.QueryEscape(value))}}
		result, err := parseListingCount(r, "p", defaultVal, ceiling)

		switch {
		case err != nil && value == "":
			t.Errorf("parseListingCount(%q) refused an absent value: %v", value, err)
		case err != nil:
			return
		case value == "" && result != defaultVal:
			t.Errorf("parseListingCount(%q, default=%d) = %d, want the default", value, defaultVal, result)
		case result < 0 || result > ceiling:
			t.Errorf("parseListingCount(%q, ceiling=%d) = %d, want within [0, ceiling]", value, ceiling, result)
		}
	})
}

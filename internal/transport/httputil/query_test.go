// -------------------------------------------------------------------------------
// HTTP Query Parameter Helper Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package httputil

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestQueryCount covers the default, clamp, and refusal branches.
func TestQueryCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw                string
		def, ceiling, want int
		refused            bool
	}{
		{"", 50, 500, 50, false},
		{"0", 50, 500, 0, false},
		{"25", 50, 500, 25, false},
		{"9000", 50, 500, 500, false},
		{"abc", 50, 500, 0, true},
		{"-5", 50, 500, 0, true},
		{"99999999999", 50, 500, 0, true},
	}
	for _, c := range cases {
		got, err := QueryCount(url.Values{"n": {c.raw}}, "n", c.def, c.ceiling)
		if got != c.want || (err != nil) != c.refused {
			t.Errorf("QueryCount(%q) = %d, %v; want %d, refused=%v", c.raw, got, err, c.want, c.refused)
		}
	}
}

// TestRequireQueryCount verifies a malformed value is answered 400 and a
// well-formed one passes through untouched.
func TestRequireQueryCount(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	req := func(target string) *http.Request {
		return httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	}
	if n, ok := RequireQueryCount(w, req("/?max=7"), "max", 0, NoCeiling); !ok || n != 7 {
		t.Errorf("RequireQueryCount(7) = %d, %v; want 7, true", n, ok)
	}
	if _, ok := RequireQueryCount(w, req("/?max=1O"), "max", 0, NoCeiling); ok || w.Code != http.StatusBadRequest {
		t.Errorf("RequireQueryCount(1O) ok=%v status=%d; want false, 400", ok, w.Code)
	}
}

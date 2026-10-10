// -------------------------------------------------------------------------------
// HTTP Query Parameter Helper Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package httputil

import "testing"

// TestQueryPositiveInt covers the values that collapse to zero.
func TestQueryPositiveInt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want int
	}{
		{"", 0},
		{"abc", 0},
		{"0", 0},
		{"-5", 0},
		{"99999999999", 0}, // beyond 32 bits
		{"25", 25},
	}
	for _, c := range cases {
		if got := QueryPositiveInt(c.raw); got != c.want {
			t.Errorf("QueryPositiveInt(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// TestQueryLimit covers the default, clamp, and parse-failure branches.
func TestQueryLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw                 string
		def, maxLimit, want int
	}{
		{"", 50, 500, 50},      // unset -> default
		{"abc", 50, 500, 50},   // unparseable -> default
		{"0", 50, 500, 50},     // non-positive -> default
		{"-5", 50, 500, 50},    // negative -> default
		{"25", 50, 500, 25},    // in range
		{"9000", 50, 500, 500}, // over cap -> clamped
	}
	for _, c := range cases {
		if got := QueryLimit(c.raw, c.def, c.maxLimit); got != c.want {
			t.Errorf("QueryLimit(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

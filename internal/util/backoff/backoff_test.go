// -------------------------------------------------------------------------------
// Backoff Tests
//
// Author: Alex Freidah
//
// Pins the doubling, the ceiling, and the counts that go straight to the
// ceiling: a negative one, and one past the point where doubling would pass
// it or overflow.
// -------------------------------------------------------------------------------

package backoff

import (
	"math"
	"testing"
	"time"
)

// TestExponential_For verifies the delay doubles from the base per attempt
// and stops at the ceiling, including a ceiling that is not a power-of-two
// multiple of the base.
func TestExponential_For(t *testing.T) {
	t.Parallel()
	minute := Exponential{Base: time.Minute, Ceiling: 24 * time.Hour}
	second := Exponential{Base: time.Second, Ceiling: 64 * time.Second}
	cases := []struct {
		name     string
		e        Exponential
		attempts int32
		want     time.Duration
	}{
		{"negative", minute, -1, 24 * time.Hour},
		{"first", minute, 0, time.Minute},
		{"doubled", minute, 3, 8 * time.Minute},
		{"below ceiling", minute, 10, 1024 * time.Minute},
		{"capped", minute, 11, 24 * time.Hour},
		{"would overflow", minute, math.MaxInt32, 24 * time.Hour},
		{"exact ceiling", second, 6, 64 * time.Second},
		{"past exact ceiling", second, 7, 64 * time.Second},
	}
	for _, tc := range cases {
		if got := tc.e.For(tc.attempts); got != tc.want {
			t.Errorf("%s: For(%d) = %v, want %v", tc.name, tc.attempts, got, tc.want)
		}
	}
}

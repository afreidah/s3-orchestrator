// -------------------------------------------------------------------------------
// CLI Output - Duration Formatting Tests
//
// Author: Alex Freidah
//
// Covers the unit-selection boundaries of FormatDuration: milliseconds under a
// second, seconds under a minute, minutes above.
// -------------------------------------------------------------------------------

package output

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "0ms"},
		{"sub-second", 340 * time.Millisecond, "340ms"},
		{"just under a second", 999 * time.Millisecond, "999ms"},
		{"exactly one second", time.Second, "1.0s"},
		{"seconds with decimal", 1200 * time.Millisecond, "1.2s"},
		{"multi-second", 2500 * time.Millisecond, "2.5s"},
		{"just under a minute", 59 * time.Second, "59.0s"},
		{"exactly one minute", time.Minute, "1.0m"},
		{"minute and a half", 90 * time.Second, "1.5m"},
		{"multi-minute", 150 * time.Second, "2.5m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FormatDuration(tc.d); got != tc.want {
				t.Errorf("FormatDuration(%s) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}

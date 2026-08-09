// -------------------------------------------------------------------------------
// Humanize Duration and Count Tests
//
// Author: Alex Freidah
//
// Table coverage for the shared renderers, including the unit boundaries where
// the output switches, since the dashboard and the TUI both read from these.
// -------------------------------------------------------------------------------

package humanize

import (
	"testing"
	"time"
)

func TestDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"zero", 0, "0s"},
		{"seconds", 5 * time.Second, "5s"},
		{"just under a minute", 59 * time.Second, "59s"},
		{"exactly a minute", time.Minute, "1m"},
		{"rounds down to minutes", 90 * time.Second, "1m"},
		{"just under an hour", 59 * time.Minute, "59m"},
		{"exactly an hour", time.Hour, "1h"},
		{"hours", 5 * time.Hour, "5h"},
		{"just under the day cutoff", 47 * time.Hour, "47h"},
		{"exactly the day cutoff", 48 * time.Hour, "2d"},
		{"days", 10 * 24 * time.Hour, "10d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Duration(tt.in); got != tt.want {
				t.Errorf("Duration(%s) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestComma(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   int64
		want string
	}{
		{"zero", 0, "0"},
		{"single digit", 7, "7"},
		{"three digits", 999, "999"},
		{"first separator", 1000, "1,000"},
		{"one leading digit", 1234, "1,234"},
		{"two leading digits", 12345, "12,345"},
		{"three leading digits", 123456, "123,456"},
		{"two separators", 1234567, "1,234,567"},
		{"a real fleet size", 132986, "132,986"},
		{"negative", -1234, "-1,234"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Comma(tt.in); got != tt.want {
				t.Errorf("Comma(%d) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

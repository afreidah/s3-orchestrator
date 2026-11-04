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

// TestElapsed pins the unit boundaries: milliseconds under a second, seconds
// under a minute, minutes above.
func TestElapsed(t *testing.T) {
	t.Parallel()
	cases := map[time.Duration]string{
		0: "0ms", 340 * time.Millisecond: "340ms", 999 * time.Millisecond: "999ms",
		time.Second: "1.0s", 1200 * time.Millisecond: "1.2s", 59 * time.Second: "59.0s",
		time.Minute: "1.0m", 90 * time.Second: "1.5m",
	}
	for d, want := range cases {
		if got := Elapsed(d); got != want {
			t.Errorf("Elapsed(%s) = %q, want %q", d, got, want)
		}
	}
}

// TestAgo pins the dash for a zero time and an age for a past one, clamped at
// zero for a time a skewed clock puts in the future.
func TestAgo(t *testing.T) {
	t.Parallel()
	if got := Ago(time.Time{}); got != "-" {
		t.Errorf("Ago(zero) = %q, want -", got)
	}
	if got := Ago(time.Now().Add(-3 * time.Hour)); got != "3h ago" {
		t.Errorf("Ago(3h) = %q, want 3h ago", got)
	}
	if got := Ago(time.Now().Add(time.Hour)); got != "0s ago" {
		t.Errorf("Ago(future) = %q, want 0s ago", got)
	}
}

// TestCount pins the singular for exactly one and grouped digits otherwise.
func TestCount(t *testing.T) {
	t.Parallel()
	cases := map[int64]string{0: "0 rows", 1: "1 row", 2: "2 rows", 1500: "1,500 rows"}
	for n, want := range cases {
		if got := Count(n, "row", "rows"); got != want {
			t.Errorf("Count(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestPercent pins whole-number percentages, past 100 for an overrun, and 0
// for a whole that is not positive.
func TestPercent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		part, whole int64
		want        int
	}{
		{0, 4096, 0}, {2048, 4096, 50}, {4096, 4096, 100}, {5000, 4096, 122}, {5, 0, 0}, {5, -1, 0},
	}
	for _, c := range cases {
		if got := Percent(c.part, c.whole); got != c.want {
			t.Errorf("Percent(%d,%d) = %d, want %d", c.part, c.whole, got, c.want)
		}
	}
}

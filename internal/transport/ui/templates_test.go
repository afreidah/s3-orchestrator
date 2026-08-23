// -------------------------------------------------------------------------------
// Template Helper Tests
//
// Author: Alex Freidah
//
// Tests for dashboard template utility functions. Validates byte formatting,
// percentage calculations, and other display helper outputs.
// -------------------------------------------------------------------------------

package ui

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// newTestCounterVec returns a fresh CounterVec backed by an isolated
// registry so the counterVecTotal test does not interact with the
// global telemetry counters. The vec is unregistered - the helper only
// needs the Collect() surface, which works on any prometheus.Collector.
func newTestCounterVec() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_counter_vec_total",
		Help: "isolated counter for counterVecTotal tests",
	}, []string{"operation"})
}

// TestCounterVecTotal exercises the snapshot helper that surfaces
// integrity check / error totals on the dashboard. Uses an isolated
// CounterVec so the test does not depend on the live telemetry
// counters or the order in which test packages register them.
func TestCounterVecTotal(t *testing.T) {
	t.Parallel()
	vec := newTestCounterVec()

	if got := counterVecTotal(vec); got != 0 {
		t.Errorf("empty vec total = %v, want 0", got)
	}

	vec.WithLabelValues("read").Inc()
	vec.WithLabelValues("read").Inc()
	vec.WithLabelValues("scrub").Inc()

	if got := counterVecTotal(vec); got != 3 {
		t.Errorf("populated vec total = %v, want 3 (2 read + 1 scrub)", got)
	}
}

// TestFormatNumber verifies the format number contract.
// Asserts that formatNumber() = , want.
func TestFormatNumber(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{1000000, "1,000,000"},
		{-42, "-42"},
		{-1234, "-1,234"},
	}

	for _, tt := range tests {
		got := formatNumber(tt.input)
		if got != tt.want {
			t.Errorf("formatNumber(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestPct verifies the pct contract.
// Asserts that pct(, ) = , want.
func TestPct(t *testing.T) {
	t.Parallel()
	tests := []struct {
		used, limit int64
		want        string
	}{
		{50, 100, "50.0%"},
		{1, 3, "33.3%"},
		{0, 100, "0.0%"},
		{100, 100, "100.0%"},
		{0, 0, "unlimited"},
		{500, 0, "unlimited"},
	}

	for _, tt := range tests {
		got := pct(tt.used, tt.limit)
		if got != tt.want {
			t.Errorf("pct(%d, %d) = %q, want %q", tt.used, tt.limit, got, tt.want)
		}
	}
}

// TestPctFloat verifies the pct float contract.
// Asserts that pctFloat(, ) = , want.
func TestPctFloat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		used, limit int64
		want        float64
	}{
		{50, 100, 50.0},
		{0, 100, 0.0},
		{100, 100, 100.0},
		{150, 100, 100.0}, // capped at 100
		{0, 0, 0.0},       // unlimited
		{500, 0, 0.0},     // unlimited
	}

	for _, tt := range tests {
		got := pctFloat(tt.used, tt.limit)
		if got != tt.want {
			t.Errorf("pctFloat(%d, %d) = %f, want %f", tt.used, tt.limit, got, tt.want)
		}
	}
}

// TestBarColor verifies the bar color contract.
// Asserts that barColor(, ) = , want.
func TestBarColor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		used, limit int64
		want        string
	}{
		{50, 100, "#22c55e"},  // green (<70%)
		{69, 100, "#22c55e"},  // green (69%)
		{70, 100, "#f59e0b"},  // amber (70%)
		{85, 100, "#f59e0b"},  // amber (85%)
		{90, 100, "#ef4444"},  // red (90%)
		{100, 100, "#ef4444"}, // red (100%)
		{0, 0, "#6b7280"},     // gray (unlimited)
		{500, 0, "#6b7280"},   // gray (unlimited)
	}

	for _, tt := range tests {
		got := barColor(tt.used, tt.limit)
		if got != tt.want {
			t.Errorf("barColor(%d, %d) = %q, want %q", tt.used, tt.limit, got, tt.want)
		}
	}
}

// TestSub covers the subtraction the savings figures rely on, including the
// negative case: an encoding that grew is a real state on a fleet where the
// ratio floor was widened after the fact, and the page must show it rather than
// clamp it to zero.
func TestSub(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b int64
		want int64
	}{
		{"a saving", 1000, 250, 750},
		{"nothing saved", 500, 500, 0},
		{"grew", 500, 600, -100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sub(tt.a, tt.b); got != tt.want {
				t.Errorf("sub(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestRatio covers the compression ratio cell, whose interesting case is an
// empty fleet: nothing measured is a dash, not a zero, since a zero there reads
// as an encoder achieving perfect compression.
func TestRatio(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b int64
		want string
	}{
		{"quarter", 250, 1000, "0.25"},
		{"no saving", 1000, 1000, "1.00"},
		{"nothing measured", 0, 0, "-"},
		{"stored without logical", 100, 0, "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ratio(tt.a, tt.b); got != tt.want {
				t.Errorf("ratio(%d, %d) = %q, want %q", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

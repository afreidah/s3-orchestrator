// -------------------------------------------------------------------------------
// Humanize - Usage Cycle Tests
//
// Author: Alex Freidah
//
// Pins how a usage cycle reads: both ends in the offset they carry, the zone
// named after them, a year end, and a dash for an unknown cycle.
// -------------------------------------------------------------------------------

package humanize

import (
	"testing"
	"time"
)

// TestCycle verifies each end reads as its own day in the offset it carries,
// so a cycle that starts at midnight Pacific is not shown a day early in UTC.
func TestCycle(t *testing.T) {
	t.Parallel()
	pacific := time.FixedZone("PDT", -7*3600)
	tests := []struct {
		name        string
		start, next time.Time
		zone        string
		want        string
	}{
		{"utc", time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), "UTC", "Oct 15 - Nov 15 UTC"},
		{"zoned", time.Date(2026, 10, 1, 0, 0, 0, 0, pacific), time.Date(2026, 11, 1, 0, 0, 0, 0, pacific), "America/Los_Angeles", "Oct 1 - Nov 1 America/Los_Angeles"},
		{"year end", time.Date(2026, 12, 14, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 14, 0, 0, 0, 0, time.UTC), "UTC", "Dec 14 - Jan 14 UTC"},
		{"unknown", time.Time{}, time.Time{}, "", "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Cycle(tt.start, tt.next, tt.zone); got != tt.want {
				t.Errorf("Cycle = %q, want %q", got, tt.want)
			}
		})
	}
}

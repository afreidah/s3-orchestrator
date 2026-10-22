// -------------------------------------------------------------------------------
// Usage Period Tests
//
// Author: Alex Freidah
//
// Pins where a backend's monthly cycle starts and ends: the calendar-month
// default, a mid-month reset day on either side of its boundary, month and year
// rollovers, and a zone whose offset changes across daylight saving time.
// -------------------------------------------------------------------------------

package core

import (
	"testing"
	"time"
)

// mustZone loads a zone by name or fails the test.
func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

// TestUsagePeriod_ZeroIsCalendarMonthUTC verifies the zero value is the
// calendar month in UTC, whatever zone the instant is given in.
func TestUsagePeriod_ZeroIsCalendarMonthUTC(t *testing.T) {
	t.Parallel()
	var p UsagePeriod
	east := time.FixedZone("east", 5*3600)
	for _, tc := range []struct {
		now   time.Time
		start string
		label string
		next  string
	}{
		{time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), "2026-10-01T00:00:00Z", "2026-10", "2026-11-01T00:00:00Z"},
		{time.Date(2026, 11, 1, 2, 0, 0, 0, east), "2026-10-01T00:00:00Z", "2026-10", "2026-11-01T00:00:00Z"},
		{time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC), "2026-12-01T00:00:00Z", "2026-12", "2027-01-01T00:00:00Z"},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), "2027-01-01T00:00:00Z", "2027-01", "2027-02-01T00:00:00Z"},
	} {
		if got := p.Start(tc.now).UTC().Format(time.RFC3339); got != tc.start {
			t.Errorf("Start(%v) = %s, want %s", tc.now, got, tc.start)
		}
		if got := p.Current(tc.now); got != tc.label {
			t.Errorf("Current(%v) = %s, want %s", tc.now, got, tc.label)
		}
		if got := p.Next(tc.now).UTC().Format(time.RFC3339); got != tc.next {
			t.Errorf("Next(%v) = %s, want %s", tc.now, got, tc.next)
		}
	}
}

// TestUsagePeriod_ResetDay verifies a cycle starting mid-month is labelled by
// the month it began, before and after its boundary and across a year end.
func TestUsagePeriod_ResetDay(t *testing.T) {
	t.Parallel()
	p := UsagePeriod{ResetDay: 14}
	for _, tc := range []struct {
		now   time.Time
		label string
		next  string
	}{
		{time.Date(2026, 10, 13, 23, 59, 59, 0, time.UTC), "2026-09", "2026-10-14T00:00:00Z"},
		{time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), "2026-10", "2026-11-14T00:00:00Z"},
		{time.Date(2027, 1, 3, 0, 0, 0, 0, time.UTC), "2026-12", "2027-01-14T00:00:00Z"},
		{time.Date(2027, 1, 20, 0, 0, 0, 0, time.UTC), "2027-01", "2027-02-14T00:00:00Z"},
	} {
		if got := p.Current(tc.now); got != tc.label {
			t.Errorf("Current(%v) = %s, want %s", tc.now, got, tc.label)
		}
		if got := p.Next(tc.now).UTC().Format(time.RFC3339); got != tc.next {
			t.Errorf("Next(%v) = %s, want %s", tc.now, got, tc.next)
		}
	}
}

// TestUsagePeriod_Location verifies the boundary is midnight in the backend's
// zone, so a provider on Pacific time rolls over eight hours after UTC, and
// seven once daylight saving time starts.
func TestUsagePeriod_Location(t *testing.T) {
	t.Parallel()
	p := UsagePeriod{Location: mustZone(t, "America/Los_Angeles")}

	// 03:00 UTC on Nov 1 is still Oct 31 in Los Angeles.
	if got := p.Current(time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)); got != "2026-10" {
		t.Errorf("Current = %s, want 2026-10 until Pacific midnight", got)
	}
	// Nov 1 starts at 07:00 UTC while PDT is still in effect.
	if got := p.Next(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)).UTC(); !got.Equal(time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("Next in October = %v, want 2026-11-01 07:00 UTC", got)
	}
	// Dec 1 starts at 08:00 UTC once PST is in effect.
	if got := p.Next(time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)).UTC(); !got.Equal(time.Date(2026, 12, 1, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("Next in November = %v, want 2026-12-01 08:00 UTC", got)
	}
}

// TestUsagePeriod_PreviousIsTheInstantBefore verifies the label of the cycle
// before the current one is the label of the instant just before Start, which
// is how a flush finds the counters of a cycle that has just ended.
func TestUsagePeriod_PreviousIsTheInstantBefore(t *testing.T) {
	t.Parallel()
	p := UsagePeriod{ResetDay: 25}
	start := p.Start(time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC))
	if got := p.Current(start.Add(-time.Nanosecond)); got != "2025-11" {
		t.Errorf("previous label = %s, want 2025-11", got)
	}
}

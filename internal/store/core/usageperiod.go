// -------------------------------------------------------------------------------
// Usage Period - When a Backend's Monthly Usage Resets
//
// Author: Alex Freidah
//
// A backend's usage is counted per monthly cycle, and providers start that
// cycle on their own day and in their own time zone. A period describes when
// its cycles begin; a cycle is one run of it, from one reset to the next.
// -------------------------------------------------------------------------------

package core

import (
	"encoding/json"
	"time"
)

// PeriodLayout is the time layout of a usage period label: the month a cycle
// began.
const PeriodLayout = "2006-01"

// UsagePeriod says when a backend's monthly usage cycle begins: at midnight on
// ResetDay in Location. The zero value is the calendar month in UTC, day 1.
type UsagePeriod struct {
	ResetDay int
	Location *time.Location
}

// UsageCycle is one run of a usage period: the instant it began and the
// instant the next begins, both in the period's zone.
type UsageCycle struct {
	Start time.Time
	Next  time.Time
}

// CycleUsage is a backend's usage so far in its current cycle, with the cycle
// it was counted in, so a figure and its label always agree.
type CycleUsage struct {
	Usage Usage
	Cycle UsageCycle
}

// Cycle returns the cycle containing now.
func (p UsagePeriod) Cycle(now time.Time) UsageCycle {
	day, loc := p.resetDay(), p.location()
	t := now.In(loc)
	start := time.Date(t.Year(), t.Month(), day, 0, 0, 0, 0, loc)
	if t.Before(start) {
		start = time.Date(t.Year(), t.Month()-1, day, 0, 0, 0, 0, loc)
	}
	return UsageCycle{Start: start, Next: start.AddDate(0, 1, 0)}
}

// Current returns the label of the cycle containing now: the month it began.
func (p UsagePeriod) Current(now time.Time) string {
	return p.Cycle(now).Start.Format(PeriodLayout)
}

// MarshalJSON renders the period as its reset day and zone name; a Location
// has no exported fields and would otherwise encode as an empty object.
func (p UsagePeriod) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ResetDay int    `json:"reset_day"`
		Timezone string `json:"timezone"`
	}{p.resetDay(), p.location().String()})
}

// Following returns the cycle after c. The reset falls at the same wall-clock
// time a month later, so it stays on the reset day across a DST change.
func (c UsageCycle) Following() UsageCycle {
	return UsageCycle{Start: c.Next, Next: c.Next.AddDate(0, 1, 0)}
}

// Zone returns the IANA name of the zone the cycle is counted in.
func (c UsageCycle) Zone() string {
	return c.Start.Location().String()
}

// resetDay is the configured day, or 1 when unset.
func (p UsagePeriod) resetDay() int {
	if p.ResetDay <= 0 {
		return 1
	}
	return p.ResetDay
}

// location is the configured zone, or UTC when unset.
func (p UsagePeriod) location() *time.Location {
	if p.Location == nil {
		return time.UTC
	}
	return p.Location
}

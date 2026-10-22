// -------------------------------------------------------------------------------
// Usage Period - When a Backend's Monthly Usage Resets
//
// Author: Alex Freidah
//
// A backend's usage is counted per monthly cycle, and providers start that
// cycle on their own day and in their own time zone. A period is labelled by
// the month its cycle began, so the label keeps the YYYY-MM form the usage
// tables and counter keys are keyed by, whatever day the cycle starts.
// -------------------------------------------------------------------------------

package core

import "time"

// PeriodLayout is the time layout of a usage period label. Its fixed width is
// what lets a counter key that embeds it be parsed back.
const PeriodLayout = "2006-01"

// UsagePeriod says when a backend's monthly usage cycle begins: at midnight on
// ResetDay in Location. The zero value is the calendar month in UTC, day 1.
type UsagePeriod struct {
	ResetDay int
	Location *time.Location
}

// Start returns the instant the cycle containing now began.
func (p UsagePeriod) Start(now time.Time) time.Time {
	day, loc := p.resetDay(), p.location()
	t := now.In(loc)
	start := time.Date(t.Year(), t.Month(), day, 0, 0, 0, 0, loc)
	if t.Before(start) {
		start = time.Date(t.Year(), t.Month()-1, day, 0, 0, 0, 0, loc)
	}
	return start
}

// Current returns the label of the cycle containing now: the month it began.
func (p UsagePeriod) Current(now time.Time) string {
	return p.Start(now).Format(PeriodLayout)
}

// Next returns the instant the cycle containing now ends and the next begins.
func (p UsagePeriod) Next(now time.Time) time.Time {
	start := p.Start(now)
	return time.Date(start.Year(), start.Month()+1, start.Day(), 0, 0, 0, 0, start.Location())
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

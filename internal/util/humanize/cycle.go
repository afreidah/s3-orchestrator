// -------------------------------------------------------------------------------
// Humanize - Usage Cycles
//
// Author: Alex Freidah
//
// Renders a backend's usage cycle the same way in the web dashboard, the TUI
// and admin status, so every surface labels a cycle in the same words.
// -------------------------------------------------------------------------------

package humanize

import "time"

// cycleDayLayout is how each end of a cycle reads: month and day.
const cycleDayLayout = "Jan 2"

// Cycle renders a usage cycle as its first day, the day it resets and its
// zone, such as "Oct 15 - Nov 15 UTC". Each end reads in the offset it carries
// over the wire, so no zone database is needed; an unknown cycle is a dash.
func Cycle(start, next time.Time, zone string) string {
	if start.IsZero() {
		return "-"
	}
	return start.Format(cycleDayLayout) + " - " + next.Format(cycleDayLayout) + " " + zone
}

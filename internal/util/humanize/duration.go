// -------------------------------------------------------------------------------
// Humanize - Durations and Counts
//
// Author: Alex Freidah
//
// Rendering helpers shared by the web dashboard, the admin CLI and the TUI so
// each presents the same figure the same way: an age, an elapsed run time, a
// share and a count with its noun.
// -------------------------------------------------------------------------------

package humanize

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration renders an age in the coarsest unit that still reads usefully,
// since these are operational spans rather than measurements.
func Duration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// Ago renders how long ago t was, as Duration words an age, and a dash for a
// zero time, which is an event that has not happened.
func Ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return Duration(max(time.Since(t), 0)) + " ago"
}

// Elapsed renders how long a run took, to the precision a run is read at:
// milliseconds under a second ("340ms"), seconds with one decimal under a
// minute ("1.2s"), and minutes with one decimal above ("2.5m").
func Elapsed(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
}

// Count renders n grouped with its noun, singular for exactly one, so a
// message naming one thing does not read as though it named several.
func Count(n int64, singular, plural string) string {
	noun := plural
	if n == 1 {
		noun = singular
	}
	return Comma(n) + " " + noun
}

// Percent returns part as a whole-number percentage of whole, and 0 when
// whole is not positive, so an unset limit or an empty total reads as 0%.
func Percent(part, whole int64) int {
	if whole <= 0 {
		return 0
	}
	return int(part * 100 / whole)
}

// Comma formats an integer with thousands separators.
func Comma(n int64) string {
	if n < 0 {
		return "-" + Comma(-n)
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	lead := len(s) % 3
	if lead == 0 {
		lead = 3
	}
	var b strings.Builder
	b.WriteString(s[:lead])
	for i := lead; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

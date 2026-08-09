// -------------------------------------------------------------------------------
// Humanize - Durations and Counts
//
// Author: Alex Freidah
//
// Rendering helpers shared by the web dashboard and the TUI so both present
// the same figure the same way.
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

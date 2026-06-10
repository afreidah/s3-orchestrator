// -------------------------------------------------------------------------------
// CLI Output - Human-Readable Durations
//
// Author: Alex Freidah
//
// Operator-facing duration formatting for text-mode output. Durations render at
// the largest unit that keeps the number readable: milliseconds under a second,
// seconds with one decimal under a minute, minutes with one decimal above.
// -------------------------------------------------------------------------------

package output

import (
	"fmt"
	"time"
)

// FormatDuration renders d for operator-facing output. Values under one second
// show milliseconds ("340ms"), under one minute show seconds with one decimal
// ("1.2s"), and above show minutes with one decimal ("2.5m").
func FormatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
}

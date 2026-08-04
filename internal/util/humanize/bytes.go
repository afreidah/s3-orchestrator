// -------------------------------------------------------------------------------
// Humanize - Byte Size Formatting
//
// Author: Alex Freidah
//
// One IEC byte formatter for every surface that renders a size to a human:
// the CLI, the TUI, the dashboard templates, and the S3 error bodies. Kept in
// a leaf package that imports nothing beyond fmt, so the transport and proxy
// layers can use it without taking a dependency on cli/ or ui/.
// -------------------------------------------------------------------------------

// Package humanize renders machine values in the units an operator reads.
package humanize

import "fmt"

// unit is the IEC step; sizes are powers of 1024, not 1000.
const unit = 1024

// prefixes indexes the IEC scale by power of 1024 above bytes.
const prefixes = "KMGTPE"

// Bytes renders a byte count in IEC units (KiB, MiB, GiB, ...) with one
// decimal place. Counts below 1024 render as plain bytes, e.g. "512 B".
//
// A negative count keeps its sign ("-4.9 KiB") rather than clamping: a
// negative here means a delta or a drifted counter, and rounding that to zero
// hides the very thing the operator needs to see. Callers whose domain
// forbids negatives should clamp before calling.
func Bytes(n int64) string {
	if n > -unit && n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v <= -unit || v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), prefixes[exp])
}

// -------------------------------------------------------------------------------
// CLI Output - Human-Readable Byte Sizes
//
// Author: Alex Freidah
//
// Formats raw byte counts in IEC units for text-mode output, so a backend limit
// reads as "10.0 GiB" rather than "10737418240". JSON mode keeps the raw count.
// -------------------------------------------------------------------------------

package output

import "fmt"

// FormatBytes renders a byte count in IEC units (KiB, MiB, GiB, ...) with one
// decimal place. Values under 1024 render as plain bytes, e.g. "512 B".
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

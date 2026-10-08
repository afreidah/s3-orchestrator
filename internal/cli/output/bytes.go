// -------------------------------------------------------------------------------
// CLI Output - Human-Readable Byte Sizes
//
// Author: Alex Freidah
//
// Formats raw byte counts in IEC units for text-mode output, so a backend limit
// reads as "10.0 GiB" rather than "10737418240". JSON mode keeps the raw count.
// -------------------------------------------------------------------------------

package output

import "github.com/afreidah/s3-orchestrator/internal/util/humanize"

// FormatBytes renders a byte count in IEC units with one decimal place, or as
// plain bytes under 1024 (e.g. "512 B").
func FormatBytes(n int64) string { return humanize.Bytes(n) }

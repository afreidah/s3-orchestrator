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

// FormatBytes renders a byte count in IEC units (KiB, MiB, GiB, ...) with one
// decimal place. Values under 1024 render as plain bytes, e.g. "512 B".
// Retained as the output package's own name so command renderers read
// consistently; the formatting itself is shared with every other surface.
func FormatBytes(n int64) string { return humanize.Bytes(n) }

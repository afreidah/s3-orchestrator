// -------------------------------------------------------------------------------
// CLI Output - Aligned Text Tables and Key/Value Blocks
//
// Author: Alex Freidah
//
// Text-mode rendering primitives shared by the CLI subcommands: a column-aligned
// table built on stdlib text/tabwriter, and a key/value block for single-record
// summaries. Both write ASCII only and compute column widths automatically so
// command-specific renderers stay declarative.
// -------------------------------------------------------------------------------

package output

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Table writes a column-aligned table to w with a header row, a dashed
// separator sized to each header, and one line per row. Column widths are
// computed by text/tabwriter. Rows shorter than the header are padded with
// blanks so ragged input still aligns.
func Table(w io.Writer, headers []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	if _, err := fmt.Fprintln(tw, strings.Join(headers, "\t")); err != nil {
		return err
	}

	dashes := make([]string, len(headers))
	for i, h := range headers {
		dashes[i] = strings.Repeat("-", len(h))
	}
	if _, err := fmt.Fprintln(tw, strings.Join(dashes, "\t")); err != nil {
		return err
	}

	for _, row := range rows {
		if _, err := fmt.Fprintln(tw, strings.Join(pad(row, len(headers)), "\t")); err != nil {
			return err
		}
	}

	return tw.Flush()
}

// pad returns row extended to n columns with empty strings.
func pad(row []string, n int) []string {
	if len(row) >= n {
		return row
	}
	out := make([]string, n)
	copy(out, row)
	return out
}

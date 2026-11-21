// -------------------------------------------------------------------------------
// Output - Page Counts
//
// Author: Alex Freidah
//
// A count taken from one listing page, as the admin CLI and the TUI state it
// before a prefix delete. A truncated page is a floor rather than a total, and
// reads as one, since understating what a delete will remove is worse than
// being vague about the exact number.
// -------------------------------------------------------------------------------

package output

import "github.com/afreidah/s3-orchestrator/internal/util/humanize"

// PageCount renders n keys counted from one listing page, stated as a floor
// when the page was truncated.
func PageCount(n int, truncated bool) string {
	count := humanize.Comma(int64(n))
	if truncated {
		return "at least " + count
	}
	return count
}

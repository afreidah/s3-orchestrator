// -------------------------------------------------------------------------------
// CLI Output - Page Count Tests
//
// Author: Alex Freidah
//
// Pins how a one-page count reads: the number itself for a whole page, and a
// floor for a truncated one, so a preview never understates a delete.
// -------------------------------------------------------------------------------

package output

import "testing"

// TestPageCount verifies a whole page states its count and a truncated page
// states it as a floor.
func TestPageCount(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		n         int
		truncated bool
		want      string
	}{
		{"a whole page", 7, false, "7"},
		{"a large page groups digits", 1000, false, "1,000"},
		{"a truncated page is a floor", 1000, true, "at least 1,000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := PageCount(tc.n, tc.truncated); got != tc.want {
				t.Errorf("PageCount(%d, %v) = %q, want %q", tc.n, tc.truncated, got, tc.want)
			}
		})
	}
}

// -------------------------------------------------------------------------------
// CLI Output - Backend Name Lists
//
// Author: Alex Freidah
//
// Renders a backend's name lists (refused calls, state flags) the same way in
// admin status and the TUI, so both surfaces describe a backend in the same
// words. The health word itself comes from core.BackendCalls.Health, which the
// web dashboard shares.
// -------------------------------------------------------------------------------

package output

import (
	"strings"
)

// NameList renders a list of names joined by commas, and a dash when it is
// empty, so a table cell is never blank.
func NameList(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ",")
}

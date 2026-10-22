// -------------------------------------------------------------------------------
// CLI Output - Backend Name Lists and Usage Limits
//
// Author: Alex Freidah
//
// Renders a backend's name lists (refused calls, state flags) and its usage
// limits the same way in admin status and the TUI, so both surfaces describe a
// backend in the same words. The health word itself comes from
// core.BackendCalls.Health, which the web dashboard shares.
// -------------------------------------------------------------------------------

package output

import (
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"
)

// MeterLabel names a usage limit: its kind, and for a request budget the
// budget it counts against, as "requests:<budget>".
func MeterLabel(kind, budget string) string {
	if budget != "" {
		return kind + ":" + budget
	}
	return kind
}

// MeterFormat returns how a usage limit's figures render: bytes for egress and
// ingress, a call count for a request budget.
func MeterFormat(kind string) func(int64) string {
	if kind == core.MeterRequests {
		return humanize.Comma
	}
	return humanize.Bytes
}

// NameList renders a list of names joined by commas, and a dash when it is
// empty, so a table cell is never blank.
func NameList(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ",")
}

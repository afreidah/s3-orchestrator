// -------------------------------------------------------------------------------
// CLI Output - Backend Name Lists and Usage Limits
//
// Author: Alex Freidah
//
// Renders a backend's name lists (refused calls, state flags), its usage limits
// and the instance reporting it the same way in admin status and the TUI, so
// both surfaces describe a backend in the same words. The health word itself
// comes from core.BackendCalls.Health, which the web dashboard shares.
// -------------------------------------------------------------------------------

package output

import (
	"fmt"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
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

// Instance names the instance that answered a status request: its id, the
// build it runs, its mode and how long it has been up. A server too old to
// report its identity renders as "unknown".
func Instance(in adminapi.InstanceStatus) string {
	if in.ID == "" {
		return "unknown"
	}
	return fmt.Sprintf("%s  %s  mode %s  up %s",
		in.ID, in.Version, in.Mode, humanize.Duration(max(time.Since(in.StartedAt), 0)))
}

// NameList renders a list of names joined by commas, and a dash when it is
// empty, so a table cell is never blank.
func NameList(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ",")
}

// -------------------------------------------------------------------------------
// CLI Output - Backend Health and Name Lists
//
// Author: Alex Freidah
//
// Renders a backend's circuit-breaker health and its name lists (refused calls,
// state flags) the same way in admin status and the TUI, so both surfaces
// describe a backend in the same words.
// -------------------------------------------------------------------------------

package output

import (
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// The backend health words. Healthy refuses no call, degraded refuses some,
// and down refuses every call.
const (
	HealthHealthy  = "healthy"
	HealthDegraded = "degraded"
	HealthDown     = "down"
)

// BackendHealth names a backend's circuit-breaker health from the calls it is
// refusing: healthy, degraded or down.
func BackendHealth(refused []string) string {
	switch len(refused) {
	case 0:
		return HealthHealthy
	case len(core.AllCalls.Names()):
		return HealthDown
	default:
		return HealthDegraded
	}
}

// NameList renders a list of names joined by commas, and a dash when it is
// empty, so a table cell is never blank.
func NameList(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ",")
}

// -------------------------------------------------------------------------------
// Admin CLI - backend state commands (drain, drain-status, drain-cancel,
// cordon, uncordon)
//
// Author: Alex Freidah
//
// Manages a backend's state. drain starts migrating copies off the named
// backend and routes new writes away, drain-status reports in-flight progress,
// and drain-cancel aborts the migration in place. cordon stops new copies
// landing on the backend without moving anything, and uncordon reverses it.
// All of them take a backend name argument and go through onBackend.
// -------------------------------------------------------------------------------

package adminctl

import (
	"fmt"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// onBackend issues method against the named backend's subpath and renders the
// response. A missing name is reported to stderr and returns exit code 1.
func onBackend(args []string, c *client, method, subpath string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, errBackendNameRequired)
		return 1
	}
	return c.do(method, adminapi.Path(adminapi.PathBackends, args[0])+subpath, nil, nil)
}

// cmdDrain implements `s3-orchestrator admin drain <backend>`. Starts a drain
// on the named backend; the operator follows up with drain-status until done.
func cmdDrain(args []string, c *client) int {
	return onBackend(args, c, http.MethodPost, adminapi.SubpathDrain)
}

// cmdDrainStatus implements `s3-orchestrator admin drain-status <backend>`.
// Returns the in-flight drain progress (objects moved, bytes moved, errors).
func cmdDrainStatus(args []string, c *client) int {
	return onBackend(args, c, http.MethodGet, adminapi.SubpathDrain)
}

// cmdDrainCancel implements `s3-orchestrator admin drain-cancel <backend>`.
// Aborts an in-flight drain; objects already migrated stay migrated.
func cmdDrainCancel(args []string, c *client) int {
	return onBackend(args, c, http.MethodDelete, adminapi.SubpathDrain)
}

// cmdCordon implements `s3-orchestrator admin cordon <backend>`. Stops new
// copies landing on the backend; it keeps serving reads, and a drain is
// unaffected.
func cmdCordon(args []string, c *client) int {
	return onBackend(args, c, http.MethodPost, adminapi.SubpathCordon)
}

// cmdUncordon implements `s3-orchestrator admin uncordon <backend>`. Lets new
// copies land on the backend again; a drain in progress carries on.
func cmdUncordon(args []string, c *client) int {
	return onBackend(args, c, http.MethodDelete, adminapi.SubpathCordon)
}

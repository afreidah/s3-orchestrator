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
// All of them require a backend name argument, validated by requireBackendName.
// -------------------------------------------------------------------------------

package adminctl

import (
	"fmt"
	"io"
)

// requireBackendName prints the missing-name error to stderr and returns
// true if args is empty (caller should bail with exit 1).
func requireBackendName(args []string, stderr io.Writer) bool {
	if len(args) == 0 {
		fmt.Fprintln(stderr, errBackendNameRequired)
		return true
	}
	return false
}

// cmdDrain implements `s3-orchestrator admin drain <backend>`. Starts a drain
// on the named backend; the operator follows up with drain-status until done.
func cmdDrain(args []string, c *client) int {
	if requireBackendName(args, c.stderr) {
		return 1
	}
	return c.post(backendPath(args[0])+drainSubpath, nil)
}

// cmdDrainStatus implements `s3-orchestrator admin drain-status <backend>`.
// Returns the in-flight drain progress (objects moved, bytes moved, errors).
func cmdDrainStatus(args []string, c *client) int {
	if requireBackendName(args, c.stderr) {
		return 1
	}
	return c.get(backendPath(args[0])+drainSubpath, nil)
}

// cmdDrainCancel implements `s3-orchestrator admin drain-cancel <backend>`.
// Aborts an in-flight drain; objects already migrated stay migrated.
func cmdDrainCancel(args []string, c *client) int {
	if requireBackendName(args, c.stderr) {
		return 1
	}
	return c.delete(backendPath(args[0]) + drainSubpath)
}

// cmdCordon implements `s3-orchestrator admin cordon <backend>`. Stops new
// copies landing on the backend; it keeps serving reads, and a drain is
// unaffected.
func cmdCordon(args []string, c *client) int {
	if requireBackendName(args, c.stderr) {
		return 1
	}
	return c.post(backendPath(args[0])+cordonSubpath, nil)
}

// cmdUncordon implements `s3-orchestrator admin uncordon <backend>`. Lets new
// copies land on the backend again; a drain in progress carries on.
func cmdUncordon(args []string, c *client) int {
	if requireBackendName(args, c.stderr) {
		return 1
	}
	return c.delete(backendPath(args[0]) + cordonSubpath)
}

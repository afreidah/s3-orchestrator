// -------------------------------------------------------------------------------
// Admin CLI - drain commands (drain, drain-status, drain-cancel)
//
// Author: Alex Freidah
//
// Manages a backend drain: drain starts migrating copies off the named backend
// and routes new writes away, drain-status reports in-flight progress, and
// drain-cancel aborts the migration in place. All three require a backend name
// argument, validated by requireBackendName.
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
	return c.post(adminBackendsPath+args[0]+drainSubpath, "", nil)
}

// cmdDrainStatus implements `s3-orchestrator admin drain-status <backend>`.
// Returns the in-flight drain progress (objects moved, bytes moved, errors).
func cmdDrainStatus(args []string, c *client) int {
	if requireBackendName(args, c.stderr) {
		return 1
	}
	return c.get(adminBackendsPath+args[0]+drainSubpath, nil)
}

// cmdDrainCancel implements `s3-orchestrator admin drain-cancel <backend>`.
// Aborts an in-flight drain; objects already migrated stay migrated.
func cmdDrainCancel(args []string, c *client) int {
	if requireBackendName(args, c.stderr) {
		return 1
	}
	return c.delete(adminBackendsPath+args[0]+drainSubpath, nil)
}

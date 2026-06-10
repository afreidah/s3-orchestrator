// -------------------------------------------------------------------------------
// Admin CLI - reconcile
//
// Author: Alex Freidah
//
// Triggers an out-of-band reconcile pass that imports untracked objects and
// removes stale rows. Without -backend it reconciles every backend.
// -------------------------------------------------------------------------------

package adminctl

import (
	"flag"
	"net/http"
)

// cmdReconcile implements `s3-orchestrator admin reconcile [-backend=NAME]`.
func cmdReconcile(args []string, c *client) int {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	backendName := fs.String("backend", "", "Scope reconcile to a single backend (default: all)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	path := "/admin/api/reconcile"
	if *backendName != "" {
		path += "?backend=" + *backendName
	}
	return c.stream(http.MethodPost, path, "")
}

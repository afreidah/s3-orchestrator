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
	"net/http"
	"net/url"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdReconcile implements `s3-orchestrator admin reconcile [-backend=NAME]`.
func cmdReconcile(args []string, c *client) int {
	fs := c.flags("reconcile")
	backendName := fs.String(flagBackend, "", "Scope reconcile to a single backend (default: all)")
	if !c.parse(fs, args) {
		return 1
	}
	q := url.Values{}
	setIf(q, queryBackend, *backendName)
	return c.stream(http.MethodPost, adminclient.WithQuery(adminapi.PathReconcile, q))
}

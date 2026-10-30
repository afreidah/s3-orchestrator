// -------------------------------------------------------------------------------
// Admin CLI - remove-backend
//
// Author: Alex Freidah
//
// Removes a backend and optionally its data. Without -purge it drops only the
// metadata rows. With -purge but no -confirm it previews what would be deleted
// from S3 storage. With both it runs the two-phase purge (fetch a confirmation
// token, then execute with it). The two-step flow guards against data loss.
// -------------------------------------------------------------------------------

package adminctl

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdRemoveBackend implements `s3-orchestrator admin remove-backend <name>
// [-purge] [-confirm]`. A purge first asks the endpoint, without a token, what
// it would destroy and for the token that authorizes it; without -confirm that
// preview is all it prints.
func cmdRemoveBackend(args []string, c *client) int {
	fs := c.flags("remove-backend")
	purge := fs.Bool("purge", false, "Also delete objects from the backend's S3 storage (requires --confirm)")
	confirm := fs.Bool("confirm", false, "Execute the purge (without this, --purge is a dry-run preview)")
	if !c.parse(fs, args) {
		return 1
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(c.stderr, errBackendNameRequired)
		return 1
	}
	name := fs.Arg(0)
	path := adminapi.Path(adminapi.PathBackends, name)
	if !*purge {
		return c.delete(path)
	}

	preview, err := c.api.Delete[adminapi.RemoveBackendPreview](context.Background(), path, url.Values{"purge": {"true"}})
	if err != nil {
		c.reportError(err)
		return 1
	}
	if !*confirm {
		//nolint:gosec // G705: stdout print of admin-CLI response, not an HTML/HTTP write  -  no XSS surface
		fmt.Fprintf(c.stdout, "Backend %q contains %d objects (%d bytes).\n", name, preview.ObjectCount, preview.TotalBytes)
		fmt.Fprintf(c.stdout, "This will permanently delete all objects from the backend's S3 storage and remove all database records.\n")
		fmt.Fprintf(c.stdout, "Re-run with --confirm to proceed.\n")
		return 0
	}
	if preview.ConfirmToken == "" {
		fmt.Fprintf(c.stderr, "error: server did not return a confirmation token\n")
		return 1
	}
	q := url.Values{"purge": {"true"}, "confirm": {preview.ConfirmToken}}
	return c.stream(http.MethodDelete, adminclient.WithQuery(path, q))
}

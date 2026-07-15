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
	"flag"
	"fmt"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdRemoveBackend implements `s3-orchestrator admin remove-backend <name>
// [-purge] [-confirm]`.
func cmdRemoveBackend(args []string, c *client) int {
	fs := flag.NewFlagSet("remove-backend", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	purge := fs.Bool("purge", false, "Also delete objects from the backend's S3 storage (requires --confirm)")
	confirm := fs.Bool("confirm", false, "Execute the purge (without this, --purge is a dry-run preview)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(c.stderr, errBackendNameRequired)
		return 1
	}
	name := fs.Arg(0)
	if !*purge {
		return c.delete(adminBackendsPath+name, nil)
	}
	if !*confirm {
		return c.removePreview(name)
	}
	return c.removePurge(name)
}

// removePreview calls the purge endpoint without confirmation and prints what
// would be destroyed.
func (c *client) removePreview(name string) int {
	path := adminBackendsPath + name + "?purge=true"
	var preview adminapi.RemoveBackendPreview
	if code := c.fetchJSON(http.MethodDelete, path, &preview); code != 0 {
		return code
	}

	//nolint:gosec // G705: stdout print of admin-CLI response, not an HTML/HTTP write  -  no XSS surface
	fmt.Fprintf(c.stdout, "Backend %q contains %d objects (%d bytes).\n", name, preview.ObjectCount, preview.TotalBytes)
	fmt.Fprintf(c.stdout, "This will permanently delete all objects from the backend's S3 storage and remove all database records.\n")
	fmt.Fprintf(c.stdout, "Re-run with --confirm to proceed.\n")
	return 0
}

// removePurge performs the two-phase purge: gets a confirmation token from the
// preview endpoint, then executes with the token.
func (c *client) removePurge(name string) int {
	path := adminBackendsPath + name + "?purge=true"
	var preview adminapi.RemoveBackendPreview
	if code := c.fetchJSON(http.MethodDelete, path, &preview); code != 0 {
		return code
	}

	if preview.ConfirmToken == "" {
		fmt.Fprintf(c.stderr, "error: server did not return a confirmation token\n")
		return 1
	}
	return c.stream(http.MethodDelete, path+"&confirm="+preview.ConfirmToken, "")
}

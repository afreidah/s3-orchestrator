// -------------------------------------------------------------------------------
// Admin CLI - object-delete
//
// Author: Alex Freidah
//
// Removes one object, every copy of it, or everything under a prefix, through
// the same admin endpoints the dashboard's file browser uses. A prefix removal
// touches many objects at once, so it is refused unless -confirm is given.
// -------------------------------------------------------------------------------

package adminctl

import (
	"fmt"
	"net/url"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdObjectDelete implements `s3-orchestrator admin object-delete` with either
// -key=<key> to remove one object or -prefix=<prefix> -confirm to remove every
// object under a prefix.
func cmdObjectDelete(args []string, c *client) int {
	fs := c.flags("object-delete")
	key := fs.String("key", "", "Object key to delete, with every copy of it")
	prefix := fs.String("prefix", "", "Delete every object under this prefix (requires -confirm)")
	confirm := fs.Bool("confirm", false, "Confirm a -prefix deletion")
	if !c.parse(fs, args) {
		return 1
	}
	switch {
	case *key != "" && *prefix != "":
		fmt.Fprintln(c.stderr, "error: -key and -prefix are mutually exclusive")
		return 1
	case *key == "" && *prefix == "":
		fmt.Fprintln(c.stderr, "error: one of -key or -prefix is required")
		return 1
	case *key != "":
		return c.delete(adminapi.Path(adminapi.PathObjects, *key))
	case !*confirm:
		fmt.Fprintf(c.stderr, "error: deleting prefix %q removes every object under it; re-run with -confirm to proceed\n", *prefix)
		return 1
	default:
		return c.delete(adminclient.WithQuery(adminapi.PathObjects, url.Values{adminapi.ParamPrefix: {*prefix}}))
	}
}

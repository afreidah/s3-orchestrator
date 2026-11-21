// -------------------------------------------------------------------------------
// Admin CLI - object-delete
//
// Author: Alex Freidah
//
// Removes one object, every copy of it, or everything under a prefix, through
// the same admin endpoints the dashboard's file browser uses. A prefix removal
// touches many objects at once, so without -confirm it only states how many it
// would remove. With -confirm it streams a line per key, so a prefix of any size
// runs to the end instead of stopping at the client's request timeout.
// -------------------------------------------------------------------------------

package adminctl

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdObjectDelete implements `s3-orchestrator admin object-delete` with either
// -key=<key> to remove one object or -prefix=<prefix> to preview, and with
// -confirm run, the removal of every object under a prefix.
func cmdObjectDelete(args []string, c *client) int {
	fs := c.flags("object-delete")
	key := fs.String("key", "", "Object key to delete, with every copy of it")
	prefix := fs.String("prefix", "", "Delete every object under this prefix (previews the count without -confirm)")
	confirm := fs.Bool("confirm", false, "Run a -prefix deletion instead of previewing it")
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
		return c.previewPrefix(*prefix)
	default:
		return c.stream(http.MethodDelete, adminclient.WithQuery(adminapi.PathObjects, url.Values{adminapi.ParamPrefix: {*prefix}}))
	}
}

// previewPrefix states how many objects a prefix delete would remove, counted
// from one flat listing page, and removes nothing.
func (c *client) previewPrefix(prefix string) int {
	q := url.Values{adminapi.ParamPrefix: {prefix}, adminapi.ParamDelimiter: {""}}
	page, err := c.api.Get[adminapi.ObjectListResponse](context.Background(), adminapi.PathObjects, q)
	if err != nil {
		c.reportError(err)
		return 1
	}
	if len(page.Objects) == 0 {
		fmt.Fprintf(c.stdout, "Nothing under %q.\n", prefix)
		return 0
	}
	fmt.Fprintf(c.stdout, "%s objects under %q would be deleted.\n", output.PageCount(len(page.Objects), page.Truncated), prefix)
	fmt.Fprintln(c.stdout, "Re-run with -confirm to proceed.")
	return 0
}

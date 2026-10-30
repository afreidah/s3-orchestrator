// -------------------------------------------------------------------------------
// Admin CLI - object-locations
//
// Author: Alex Freidah
//
// Looks up the per-backend ledger for a single object key so an operator can
// see exactly which backends hold a copy and at what size.
// -------------------------------------------------------------------------------

package adminctl

import (
	"net/url"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdObjectLocations implements `s3-orchestrator admin object-locations
// -key=<key>`.
func cmdObjectLocations(args []string, c *client) int {
	fs := c.flags("object-locations")
	key := fs.String("key", "", "Object key to look up (required)")
	if !c.parse(fs, args, "key") {
		return 1
	}
	return c.get(adminclient.WithQuery(adminapi.PathObjectLocations, url.Values{"key": {*key}}), nil)
}

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
	"flag"
	"fmt"
)

// cmdObjectLocations implements `s3-orchestrator admin object-locations
// -key=<key>`.
func cmdObjectLocations(args []string, c *client) int {
	fs := flag.NewFlagSet("object-locations", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	key := fs.String("key", "", "Object key to look up (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *key == "" {
		fmt.Fprintln(c.stderr, "error: -key is required")
		return 1
	}
	return c.get("/admin/api/object-locations?key="+*key, nil)
}

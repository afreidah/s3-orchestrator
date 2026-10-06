// -------------------------------------------------------------------------------
// Admin CLI - unreadable
//
// Author: Alex Freidah
//
// Lists copies that are encrypted with no key, or deletes all of them when
// -execute is passed.
// -------------------------------------------------------------------------------

package adminctl

import (
	"flag"
	"fmt"
	"net/http"
)

// cmdUnreadable implements `s3-orchestrator admin unreadable [-limit=N]
// [-execute] [-batch-size=N]`.
func cmdUnreadable(args []string, c *client) int {
	fs := flag.NewFlagSet("unreadable", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	execute := fs.Bool("execute", false, "Delete every unreadable copy (default: list them)")
	limit := fs.Int("limit", 0, "Maximum copies to list")
	batchSize := fs.Int(flagBatchSize, 0, "Copies purged per pass")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *execute {
		path := "/admin/api/unreadable"
		if *batchSize > 0 {
			path += fmt.Sprintf(fmtBatchSize, *batchSize)
		}
		return c.stream(http.MethodPost, path, "")
	}
	path := "/admin/api/unreadable"
	if *limit > 0 {
		path += fmt.Sprintf("?limit=%d", *limit)
	}
	return c.get(path, nil)
}

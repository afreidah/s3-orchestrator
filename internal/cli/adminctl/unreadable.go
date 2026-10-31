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
	"net/http"
	"net/url"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdUnreadable implements `s3-orchestrator admin unreadable [-limit=N]
// [-execute] [-batch-size=N]`.
func cmdUnreadable(args []string, c *client) int {
	fs := c.flags("unreadable")
	execute := fs.Bool("execute", false, "Delete every unreadable copy (default: list them)")
	limit := fs.Int("limit", 0, "Maximum copies to list")
	batchSize := fs.Int(flagBatchSize, 0, "Copies purged per pass")
	if !c.parse(fs, args) {
		return 1
	}
	q := url.Values{}
	if *execute {
		setPositive(q, queryBatch, *batchSize)
		return c.stream(http.MethodPost, adminclient.WithQuery(adminapi.PathUnreadable, q))
	}
	setPositive(q, "limit", *limit)
	return c.get(adminclient.WithQuery(adminapi.PathUnreadable, q), nil)
}

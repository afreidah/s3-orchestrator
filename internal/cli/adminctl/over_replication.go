// -------------------------------------------------------------------------------
// Admin CLI - over-replication
//
// Author: Alex Freidah
//
// Shows the current over-replicated (pending-excess) count, or runs the
// over-replication cleaner once with an optional batch-size override when
// -execute is passed.
// -------------------------------------------------------------------------------

package adminctl

import (
	"net/http"
	"net/url"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdOverReplication implements `s3-orchestrator admin over-replication
// [-execute] [-batch-size=N]`.
func cmdOverReplication(args []string, c *client) int {
	fs := c.flags("over-replication")
	execute := fs.Bool("execute", false, "Run cleanup (default: show status only)")
	batchSize := fs.Int(flagBatchSize, 0, "Override batch size for cleanup")
	if !c.parse(fs, args) {
		return 1
	}
	if *execute {
		q := url.Values{}
		setPositive(q, queryBatch, *batchSize)
		return c.stream(http.MethodPost, adminclient.WithQuery(adminapi.PathOverReplication, q))
	}
	return c.get(adminapi.PathOverReplication, nil)
}

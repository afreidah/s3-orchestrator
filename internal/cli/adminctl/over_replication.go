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
	"flag"
	"net/http"
	"net/url"
	"strconv"
)

// cmdOverReplication implements `s3-orchestrator admin over-replication
// [-execute] [-batch-size=N]`.
func cmdOverReplication(args []string, c *client) int {
	fs := flag.NewFlagSet("over-replication", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	execute := fs.Bool("execute", false, "Run cleanup (default: show status only)")
	batchSize := fs.Int(flagBatchSize, 0, "Override batch size for cleanup")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *execute {
		q := url.Values{}
		if *batchSize > 0 {
			q.Set(queryBatch, strconv.Itoa(*batchSize))
		}
		return c.stream(http.MethodPost, withQuery("/admin/api/over-replication", q))
	}
	return c.get("/admin/api/over-replication", nil)
}

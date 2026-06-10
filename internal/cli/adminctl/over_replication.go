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
	"fmt"
	"net/http"
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
		path := "/admin/api/over-replication"
		if *batchSize > 0 {
			path += fmt.Sprintf(fmtBatchSize, *batchSize)
		}
		return c.stream(http.MethodPost, path, "")
	}
	return c.get("/admin/api/over-replication", nil)
}

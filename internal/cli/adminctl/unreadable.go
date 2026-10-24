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
	"net/http"
	"net/url"
	"strconv"
)

// unreadablePath is the endpoint both the listing and the purge address.
const unreadablePath = "/admin/api/unreadable"

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
	q := url.Values{}
	if *execute {
		if *batchSize > 0 {
			q.Set(queryBatch, strconv.Itoa(*batchSize))
		}
		return c.stream(http.MethodPost, withQuery(unreadablePath, q))
	}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	return c.get(withQuery(unreadablePath, q), nil)
}

// -------------------------------------------------------------------------------
// Admin CLI - scrub
//
// Author: Alex Freidah
//
// Triggers one scrubber pass over the copies least recently verified, or with
// -key, verifies every copy of one object immediately. -batch-size overrides
// the configured default for a single pass.
// -------------------------------------------------------------------------------

package adminctl

import (
	"flag"
	"net/http"
	"net/url"
	"strconv"
)

// cmdScrub implements `s3-orchestrator admin scrub [-batch-size=N] [-key=KEY]`.
//
// With -key the sweep is bypassed entirely: the named object's copies are
// verified now and reported one per line. Waiting for the queue to reach a
// specific key can take days on a large fleet, which is no use when an operator
// is asking about one object because something already looks wrong.
func cmdScrub(args []string, c *client) int {
	fs := flag.NewFlagSet("scrub", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	batchSize := fs.Int(flagBatchSize, 0, "Number of objects to verify (0 = use server default)")
	key := fs.String("key", "", "Verify every copy of this object now instead of running a pass")
	backend := fs.String(flagBackend, "", usageBackend)
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if *key != "" {
		return c.post("/admin/api/object-scrub?key="+url.QueryEscape(*key), "", nil)
	}

	q := url.Values{}
	if *batchSize > 0 {
		q.Set("batch_size", strconv.Itoa(*batchSize))
	}
	if *backend != "" {
		q.Set(queryBackend, *backend)
	}
	return c.stream(http.MethodPost, withQuery("/admin/api/scrub", q), "")
}

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
	"net/http"
	"net/url"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdScrub implements `s3-orchestrator admin scrub [-batch-size=N] [-key=KEY]`.
// With -key the sweep is bypassed: the named object's copies are verified
// immediately and reported one per line.
func cmdScrub(args []string, c *client) int {
	fs := c.flags("scrub")
	batchSize := fs.Int(flagBatchSize, 0, "Number of objects to verify (0 = use server default)")
	key := fs.String("key", "", "Verify every copy of this object now instead of running a pass")
	backend := fs.String(flagBackend, "", usageBackend)
	if !c.parse(fs, args) {
		return 1
	}

	if *key != "" {
		return c.post(adminclient.WithQuery(adminapi.PathObjectScrub, url.Values{"key": {*key}}), nil)
	}

	q := url.Values{}
	setPositive(q, queryBatch, *batchSize)
	setIf(q, queryBackend, *backend)
	return c.stream(http.MethodPost, adminclient.WithQuery(adminapi.PathScrub, q))
}

// -------------------------------------------------------------------------------
// Admin CLI - shared bulk-rewrite command runner
//
// Author: Alex Freidah
//
// The flag parsing and streamed POST every fleet-wide rewrite command shares.
// Compression and encryption differ only in which endpoint they call, so they
// take the same flags rather than each growing its own set.
// -------------------------------------------------------------------------------

package adminctl

import (
	"net/http"
	"net/url"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// runBulkRewrite parses the shared flag set and streams one pass, which a fixed
// client timeout would cut off. -max stops after that many objects; repeated
// runs resume where the last one stopped, because converted and ratio-declined
// copies leave the selection. -backend limits the pass to one backend's copies.
func runBulkRewrite(args []string, c *client, name, path string) int {
	fs := c.flags(name)
	maxObjects := fs.Int(flagMax, 0, "Stop after rewriting this many objects (0 = the whole fleet)")
	backend := fs.String(flagBackend, "", usageBackend)
	if !c.parse(fs, args) {
		return 1
	}

	q := url.Values{}
	setPositive(q, flagMax, *maxObjects)
	setIf(q, adminapi.ParamBackend, *backend)
	return c.stream(http.MethodPost, adminclient.WithQuery(path, q))
}

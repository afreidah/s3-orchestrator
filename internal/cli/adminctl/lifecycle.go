// -------------------------------------------------------------------------------
// Admin CLI - lifecycle command
//
// Author: Alex Freidah
//
// Runs one expiration sweep on demand. The scheduled sweep is hourly plus
// startup jitter, which is a long time to wait to find out whether a rule you
// just wrote matches anything.
// -------------------------------------------------------------------------------

package adminctl

import "net/http"

// cmdLifecycle implements `s3-orchestrator admin lifecycle`. Streams a line per
// expired object, then the total.
func cmdLifecycle(_ []string, c *client) int {
	return c.stream(http.MethodPost, "/admin/api/lifecycle", "")
}

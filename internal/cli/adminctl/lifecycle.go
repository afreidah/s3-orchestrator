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

// cmdLifecycle implements `s3-orchestrator admin lifecycle`. Applies every
// configured lifecycle rule once and prints what it deleted.
func cmdLifecycle(_ []string, c *client) int {
	return c.post("/admin/api/lifecycle", "", nil)
}

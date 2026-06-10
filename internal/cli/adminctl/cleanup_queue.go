// -------------------------------------------------------------------------------
// Admin CLI - cleanup-queue
//
// Author: Alex Freidah
//
// Reports the pending-cleanup depth and a sample of pending items so an
// operator can spot stuck retries before they exhaust to the DLQ.
// -------------------------------------------------------------------------------

package adminctl

// cmdCleanupQueue implements `s3-orchestrator admin cleanup-queue`.
func cmdCleanupQueue(_ []string, c *client) int {
	return c.get("/admin/api/cleanup-queue", nil)
}

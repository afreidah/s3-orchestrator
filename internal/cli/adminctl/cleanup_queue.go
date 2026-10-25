// -------------------------------------------------------------------------------
// Admin CLI - cleanup-queue
//
// Author: Alex Freidah
//
// Reports the pending-cleanup depth and a sample of pending items so an
// operator can spot stuck retries before they exhaust to the DLQ.
// -------------------------------------------------------------------------------

package adminctl

import "github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

// cmdCleanupQueue implements `s3-orchestrator admin cleanup-queue`.
func cmdCleanupQueue(_ []string, c *client) int {
	return c.get(adminapi.PathCleanupQueue, nil)
}

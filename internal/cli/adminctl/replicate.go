// -------------------------------------------------------------------------------
// Admin CLI - replicate
//
// Author: Alex Freidah
//
// Triggers the replicator background worker on demand instead of waiting for
// the next scheduled tick. Useful right after a drain when the operator wants
// to converge replicas immediately. replication-rebuild rebuilds the queue the
// replicator drains from the object ledger.
// -------------------------------------------------------------------------------

package adminctl

import (
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdReplicate implements `s3-orchestrator admin replicate`.
func cmdReplicate(_ []string, c *client) int {
	return c.stream(http.MethodPost, adminapi.PathReplicate)
}

// cmdReplicationRebuild implements `s3-orchestrator admin replication-rebuild`.
func cmdReplicationRebuild(_ []string, c *client) int {
	return c.post(adminapi.PathReplicationRebuild, nil)
}

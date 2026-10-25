// -------------------------------------------------------------------------------
// Admin CLI - replicate
//
// Author: Alex Freidah
//
// Triggers the replicator background worker on demand instead of waiting for
// the next scheduled tick. Useful right after a drain when the operator wants
// to converge replicas immediately.
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

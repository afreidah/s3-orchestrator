// -------------------------------------------------------------------------------
// Admin CLI - status
//
// Author: Alex Freidah
//
// Reports per-backend health, quota usage, and circuit-breaker state from the
// running instance. Text mode renders the backends as a table led by the
// backend name; JSON mode returns the raw payload.
// -------------------------------------------------------------------------------

package adminctl

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdStatus implements `s3-orchestrator admin status`.
func cmdStatus(_ []string, c *client) int {
	return c.get("/admin/api/status", renderStatus)
}

// renderStatus renders the status payload as a backends table led by the
// backend name, followed by the DB health and usage period. Returning an error
// (e.g. an unexpected shape) makes the client fall back to raw JSON.
func renderStatus(w io.Writer, body []byte) error {
	var s adminapi.StatusResponse
	if err := json.Unmarshal(body, &s); err != nil {
		return err
	}

	headers := []string{"Backend", "Health", "State", "Used", "Limit", "Objects", "API reqs", "Ingress", "Egress"}
	rows := make([][]string, len(s.Backends))
	for i, b := range s.Backends {
		rows[i] = []string{
			b.Name,
			health(b.Healthy),
			backendState(b.Flags),
			output.FormatBytes(b.BytesUsed),
			output.FormatBytes(b.BytesLimit),
			strconv.FormatInt(b.ObjectCount, 10),
			strconv.FormatInt(b.APIRequests, 10),
			output.FormatBytes(b.IngressBytes),
			output.FormatBytes(b.EgressBytes),
		}
	}
	if err := output.Table(w, headers, rows); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\nDB healthy:    %t\nUsage period:  %s\n", s.DBHealthy, s.UsagePeriod)
	return err
}

// health renders a backend's circuit-breaker state for the status table.
func health(healthy bool) string {
	if healthy {
		return "healthy"
	}
	return "unhealthy"
}

// backendState renders a backend's state flags for the status table, joined by
// commas, and a dash for a backend in normal service.
func backendState(flags []string) string {
	if len(flags) == 0 {
		return "-"
	}
	return strings.Join(flags, ",")
}

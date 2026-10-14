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
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdStatus implements `s3-orchestrator admin status`, fetching the status
// payload and rendering it as a table.
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

	headers := []string{"Backend", "Health", "Refused", "State", "Used", "Limit", "Objects", "API reqs", "Ingress", "Egress"}
	rows := make([][]string, len(s.Backends))
	for i := range s.Backends {
		b := &s.Backends[i]
		rows[i] = []string{
			b.Name,
			health(b.RefusedCalls),
			nameList(b.RefusedCalls),
			nameList(b.Flags),
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

// health renders a backend's circuit-breaker state for the status table:
// healthy while it refuses nothing, down while it refuses every call, and
// degraded in between.
func health(refused []string) string {
	switch len(refused) {
	case 0:
		return "healthy"
	case len(core.AllCalls.Names()):
		return "down"
	default:
		return "degraded"
	}
}

// nameList renders a list of names for the status table, joined by commas,
// and a dash when it is empty.
func nameList(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ",")
}

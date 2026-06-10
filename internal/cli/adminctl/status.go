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

	"github.com/afreidah/s3-orchestrator/internal/cli/output"
)

// statusResponse is the subset of the /admin/api/status payload the text
// renderer reads. JSON mode bypasses this and prints the raw body.
type statusResponse struct {
	Backends []struct {
		Name         string `json:"name"`
		BytesUsed    int64  `json:"bytes_used"`
		BytesLimit   int64  `json:"bytes_limit"`
		ObjectCount  int64  `json:"object_count"`
		APIRequests  int64  `json:"api_requests"`
		IngressBytes int64  `json:"ingress_bytes"`
		EgressBytes  int64  `json:"egress_bytes"`
	} `json:"backends"`
	DBHealthy   bool   `json:"db_healthy"`
	UsagePeriod string `json:"usage_period"`
}

// cmdStatus implements `s3-orchestrator admin status`.
func cmdStatus(_ []string, c *client) int {
	return c.get("/admin/api/status", renderStatus)
}

// renderStatus renders the status payload as a backends table led by the
// backend name, followed by the DB health and usage period. Returning an error
// (e.g. an unexpected shape) makes the client fall back to raw JSON.
func renderStatus(w io.Writer, body []byte) error {
	var s statusResponse
	if err := json.Unmarshal(body, &s); err != nil {
		return err
	}

	headers := []string{"Backend", "Used", "Limit", "Objects", "API reqs", "Ingress", "Egress"}
	rows := make([][]string, len(s.Backends))
	for i, b := range s.Backends {
		rows[i] = []string{
			b.Name,
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

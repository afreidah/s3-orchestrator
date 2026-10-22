// -------------------------------------------------------------------------------
// Admin CLI - status
//
// Author: Alex Freidah
//
// Reports per-backend health, quota usage, usage limits, and circuit-breaker
// state from the running instance. Text mode renders the backends as a table
// led by the backend name, then a table of usage limits when any backend has
// one; JSON mode returns the raw payload.
// -------------------------------------------------------------------------------

package adminctl

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"
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
			core.CallsNamed(b.RefusedCalls).Health(),
			output.NameList(b.RefusedCalls),
			output.NameList(b.Flags),
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
	if err := renderUsageLimits(w, s.Backends); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\nDB healthy:    %t\nUsage period:  %s\n", s.DBHealthy, s.UsagePeriod)
	return err
}

// renderUsageLimits renders every usage limit the backends enforce, one row
// each, with what the period has used of it. A fleet with no limits prints
// nothing.
func renderUsageLimits(w io.Writer, backends []adminapi.BackendStatus) error {
	var rows [][]string
	for i := range backends {
		for _, u := range backends[i].UsageLimits {
			format := output.MeterFormat(u.Kind)
			rows = append(rows, []string{
				backends[i].Name,
				output.MeterLabel(u.Kind, u.Budget),
				format(u.Used),
				format(u.Limit),
				fmt.Sprintf("%d%%", humanize.Percent(u.Used, u.Limit)),
			})
		}
	}
	if len(rows) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return output.Table(w, []string{"Backend", "Usage limit", "Used", "Limit", "Use%"}, rows)
}

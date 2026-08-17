// -------------------------------------------------------------------------------
// Admin API - Logs
//
// Author: Alex Freidah
//
// Serves the in-memory structured-log ring buffer (the same source the web
// dashboard's logs pane reads) so the TUI, which authenticates with the admin
// token rather than a UI session, can show recent activity.
// -------------------------------------------------------------------------------

package admin

import (
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// logReader is the narrow view of the log ring buffer the handler needs; a real
// *telemetry.LogBuffer satisfies it, and tests pass a fake.
type logReader interface {
	Entries(opts *telemetry.LogQueryOpts) []telemetry.LogEntry
}

// defaultLogLimit bounds a logs page when the caller supplies none;
// maxLogLimit caps it so one request cannot pull the whole buffer.
const (
	defaultLogLimit = 200
	maxLogLimit     = 1000
)

// handleLogs returns recent structured log entries, optionally filtered by
// minimum level (?level=) and bounded by ?limit=. Returns 503 when the log
// buffer was not wired (e.g. a deployment that disabled it).
func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	if h.logs == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "log buffer not available")
		return
	}
	opts := telemetry.LogQueryOpts{
		MinLevel: telemetry.ParseLevel(r.URL.Query().Get("level")),
		Limit:    parseLimit(r.URL.Query().Get("limit"), defaultLogLimit, maxLogLimit),
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.LogsResponse{
		Entries: logEntries(h.logs.Entries(&opts)),
	})
}

// logEntries maps buffer records onto the shared wire type, lifting the
// "component" attribute into its own field and passing the remaining
// attributes through so the client can render a full, human-readable line.
func logEntries(entries []telemetry.LogEntry) []adminapi.LogEntry {
	out := make([]adminapi.LogEntry, 0, len(entries))
	for i := range entries {
		component, _ := entries[i].Attrs["component"].(string)
		out = append(out, adminapi.LogEntry{
			Time:      entries[i].Time,
			Level:     entries[i].Level,
			Message:   entries[i].Message,
			Component: component,
			Attrs:     attrsExceptComponent(entries[i].Attrs),
		})
	}
	return out
}

// attrsExceptComponent copies attrs without the "component" key (which is
// surfaced in its own field), returning nil when nothing remains.
func attrsExceptComponent(attrs map[string]any) map[string]any {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]any, len(attrs))
	for k, v := range attrs {
		if k != "component" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

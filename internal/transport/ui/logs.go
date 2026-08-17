// -------------------------------------------------------------------------------
// UI Handler - Logs API
//
// Author: Alex Freidah
//
// Serves the in-memory log ring buffer to the dashboard's logs pane.
// Supports level/since/before/component/limit query parameters; the
// limit handling fetches one extra entry to detect "more available"
// without an extra DB round-trip. All RFC3339 timestamp parses are
// best-effort - unparseable values become the zero time, which the log
// buffer treats as "no bound".
// -------------------------------------------------------------------------------

package ui

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// logsResponse wraps log entries with pagination metadata.
type logsResponse struct {
	Entries []telemetry.LogEntry `json:"entries"`
	HasMore bool                 `json:"hasMore"`
}

// handleAPILogs returns buffered log entries as JSON. Supports query
// parameters for filtering: level (minimum severity), since (RFC3339
// timestamp), before (RFC3339 timestamp for pagination), component, and
// limit.
func (h *Handler) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	opts, requestedLimit := buildLogQueryOpts(r.URL.Query())
	entries := h.logBuffer.Entries(&opts)
	if entries == nil {
		entries = []telemetry.LogEntry{}
	}

	resp := logsResponse{Entries: entries}
	if requestedLimit > 0 && len(entries) > requestedLimit {
		resp.Entries = entries[:requestedLimit]
		resp.HasMore = true
	}

	httputil.WriteJSON(w, http.StatusOK, resp)
}

// buildLogQueryOpts parses log-API query parameters into LogQueryOpts.
// Returns the parsed options and the client's requested limit (0 when
// unset). The Limit on opts is the client limit plus one so the handler
// can detect whether more entries exist.
func buildLogQueryOpts(q url.Values) (telemetry.LogQueryOpts, int) {
	opts := telemetry.LogQueryOpts{}
	opts.MinLevel = telemetry.ParseLevel(q.Get("level"))
	opts.Since = parseLogTimestamp(q.Get("since"))
	opts.Before = parseLogTimestamp(q.Get("before"))
	opts.Component = q.Get("component")

	requestedLimit := parseLogLimit(q.Get("limit"))
	if requestedLimit > 0 {
		opts.Limit = requestedLimit + 1
	}
	return opts, requestedLimit
}

// parseLogTimestamp parses an RFC3339 timestamp into a time.Time. Empty
// or unparseable input returns the zero value, which the log buffer
// treats as "no bound".
func parseLogTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseLogLimit parses the requested page size. Non-positive or
// unparseable values disable client-side limiting (return 0).
func parseLogLimit(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

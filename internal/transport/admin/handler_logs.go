// -------------------------------------------------------------------------------
// Admin API - Logs
//
// Author: Alex Freidah
//
// Serves the in-memory structured-log ring buffer. The admin API mounts it for
// the TUI, which signs its requests with SigV4, and the web dashboard mounts
// the same handler behind its session auth, so both surfaces share one query
// contract and one wire shape.
// -------------------------------------------------------------------------------

package admin

import (
	"cmp"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// LogReader is the narrow view of the log ring buffer the handler needs; a real
// *telemetry.LogBuffer satisfies it, and tests pass a fake.
type LogReader interface {
	Entries(opts *telemetry.LogQueryOpts) []telemetry.LogEntry
}

// defaultLogLimit bounds a logs page when the caller supplies none;
// maxLogLimit caps it so one request cannot pull the whole buffer.
const (
	defaultLogLimit = 200
	maxLogLimit     = 1000
)

// LogsHandler returns recent structured log entries, oldest first, filtered by
// minimum level (?level=), time window (?since=, ?before=, RFC3339) and
// component (?component=), and bounded by ?limit=. HasMore reports that older
// matching entries were cut by the limit. Returns 503 when logs is nil (a
// deployment that disabled the buffer).
func LogsHandler(logs LogReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if logs == nil {
			httputil.WriteJSONError(w, http.StatusServiceUnavailable, "log buffer not available")
			return
		}
		opts, err := logQueryOpts(r.URL.Query())
		if err != nil {
			httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		limit := opts.Limit
		// Fetch one past the limit so a cut page can say older entries exist.
		opts.Limit++
		entries := logs.Entries(&opts)

		var resp adminapi.LogsResponse
		if len(entries) > limit {
			entries = entries[len(entries)-limit:]
			resp.HasMore = true
		}
		resp.Entries = logEntries(entries)
		httputil.WriteJSON(w, http.StatusOK, resp)
	}
}

// logQueryOpts parses the logs query parameters into buffer query options,
// refusing a malformed value rather than widening the query by dropping it.
func logQueryOpts(q url.Values) (telemetry.LogQueryOpts, error) {
	level, levelErr := config.ParseLogLevel(q.Get("level"))
	since, sinceErr := parseLogTimestamp(q, "since")
	before, beforeErr := parseLogTimestamp(q, "before")
	limit, limitErr := httputil.QueryCount(q, "limit", defaultLogLimit, maxLogLimit)
	return telemetry.LogQueryOpts{
		MinLevel:  level,
		Since:     since,
		Before:    before,
		Component: q.Get("component"),
		Limit:     limit,
	}, cmp.Or(levelErr, sinceErr, beforeErr, limitErr)
}

// parseLogTimestamp parses the RFC3339 timestamp in param. An absent value
// returns the zero time, which the log buffer treats as no bound.
func parseLogTimestamp(q url.Values, param string) (time.Time, error) {
	s := q.Get(param)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC3339 timestamp", param)
	}
	return t, nil
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
			Level:     entries[i].Level.String(),
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

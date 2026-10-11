// -------------------------------------------------------------------------------
// Admin API - Shared Log DTOs
//
// Author: Alex Freidah
//
// Wire types for the logs endpoint shared by the handler, the TUI logs pane and
// the web dashboard's logs pane. Kept in the leaf adminapi package so the server and its client depend on
// one definition and the JSON shape cannot drift.
// -------------------------------------------------------------------------------

package adminapi

import "time"

// LogsResponse is a page of recent structured log entries, oldest first.
// HasMore reports that older matching entries were cut by the page limit.
type LogsResponse struct {
	Entries []LogEntry `json:"entries"`
	HasMore bool       `json:"hasMore"`
}

// LogEntry is one structured log record: the timestamp, severity, message, the
// emitting component (lifted from the attributes for its own column), and the
// remaining structured attributes so the client can render a full,
// human-readable line rather than a bare message.
type LogEntry struct {
	Time      time.Time      `json:"time"`
	Level     string         `json:"level"`
	Message   string         `json:"message"`
	Component string         `json:"component,omitempty"`
	Attrs     map[string]any `json:"attrs,omitempty"`
}

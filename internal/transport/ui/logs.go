// -------------------------------------------------------------------------------
// UI Handler - Logs API
//
// Author: Alex Freidah
//
// Serves the in-memory log ring buffer to the dashboard's logs pane through the
// admin API's logs handler, so the dashboard and the TUI share one query
// contract and one wire shape. Only the security headers are added here.
// -------------------------------------------------------------------------------

package ui

import "net/http"

// handleAPILogs returns buffered log entries as JSON via the shared handler.
func (h *Handler) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	h.logs(w, r)
}

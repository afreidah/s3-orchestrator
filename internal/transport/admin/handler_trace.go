// -------------------------------------------------------------------------------
// Admin API - runtime/trace.FlightRecorder Snapshot
//
// Author: Alex Freidah
//
// POST /admin/api/trace/snapshot streams the current trace ring buffer to
// the client as application/octet-stream. The output is consumable by
// `go tool trace <file>`. Disabled (503) unless debug.flight_recorder is
// turned on in config.
// -------------------------------------------------------------------------------

package admin

import (
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// handleTraceSnapshot streams the FlightRecorder ring buffer as a binary
// trace file. The file is whatever `go tool trace` accepts.
func (h *Handler) handleTraceSnapshot(w http.ResponseWriter, r *http.Request) {
	if h.flightRec == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "flight recorder is disabled (set debug.flight_recorder.enabled: true)")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="trace.bin"`)
	if _, err := h.flightRec.WriteTo(w); err != nil {
		// Headers are likely already flushed; log instead of writing a
		// late JSON error that would corrupt the binary stream.
		h.log.WarnContext(r.Context(), "flight recorder snapshot failed", "error", err)
	}
}

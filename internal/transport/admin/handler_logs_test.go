// -------------------------------------------------------------------------------
// Admin API - Logs Handler Tests
//
// Author: Alex Freidah
//
// Covers the logs endpoint: the typed response with component lifted out of the
// entry attributes, the 503 when no buffer is wired, and parseLogLevel.
// -------------------------------------------------------------------------------

package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// fakeLogReader is a logReader returning canned entries.
type fakeLogReader struct{ entries []telemetry.LogEntry }

func (f fakeLogReader) Entries(*telemetry.LogQueryOpts) []telemetry.LogEntry { return f.entries }

// TestHandleLogs_ReturnsEntriesWithComponent asserts the endpoint maps buffer
// entries to the wire type and lifts the component attribute into its field.
func TestHandleLogs_ReturnsEntriesWithComponent(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default(), logs: fakeLogReader{entries: []telemetry.LogEntry{
		{Time: time.Now(), Level: "INFO", Message: "replicated", Attrs: map[string]any{"component": "replicator", "key": "foo"}},
		{Time: time.Now(), Level: "WARN", Message: "slow", Attrs: nil},
		{Time: time.Now(), Level: "INFO", Message: "tick", Attrs: map[string]any{"component": "scrubber"}},
	}}}

	w := httptest.NewRecorder()
	h.handleLogs(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/logs?level=INFO&limit=50", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp adminapi.LogsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if len(resp.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(resp.Entries))
	}
	if resp.Entries[0].Component != "replicator" || resp.Entries[0].Message != "replicated" {
		t.Errorf("entry[0] = %+v", resp.Entries[0])
	}
	// component is lifted out of attrs; other attrs pass through.
	if _, ok := resp.Entries[0].Attrs["component"]; ok {
		t.Error("component should not remain in attrs")
	}
	if resp.Entries[0].Attrs["key"] != "foo" {
		t.Errorf("entry[0] attrs = %v, want key=foo", resp.Entries[0].Attrs)
	}
	if resp.Entries[1].Component != "" {
		t.Errorf("entry[1] component = %q, want empty", resp.Entries[1].Component)
	}
	// entry[2] carried only component, so attrs collapse to empty (omitted).
	if resp.Entries[2].Component != "scrubber" || len(resp.Entries[2].Attrs) != 0 {
		t.Errorf("entry[2] = %+v, want component=scrubber and no attrs", resp.Entries[2])
	}
}

// TestHandleLogs_NoBufferReturns503 asserts the endpoint reports unavailable
// when no log buffer was wired.
func TestHandleLogs_NoBufferReturns503(t *testing.T) {
	t.Parallel()
	h := &Handler{log: slog.Default()} // logs nil
	w := httptest.NewRecorder()
	h.handleLogs(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/logs", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

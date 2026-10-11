// -------------------------------------------------------------------------------
// Admin API - Logs Handler Tests
//
// Author: Alex Freidah
//
// Covers the shared logs handler: the typed response with component lifted out
// of the entry attributes, the query filters, the limit keeping the newest
// entries with HasMore set, and the 503 when no buffer is wired.
// -------------------------------------------------------------------------------

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// fakeLogReader is a LogReader returning canned entries.
type fakeLogReader struct{ entries []telemetry.LogEntry }

func (f fakeLogReader) Entries(*telemetry.LogQueryOpts) []telemetry.LogEntry { return f.entries }

// getLogs drives the handler over logs with the given query and decodes the
// response.
func getLogs(t *testing.T, logs LogReader, query string) adminapi.LogsResponse {
	t.Helper()
	w := httptest.NewRecorder()
	LogsHandler(logs)(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/logs?"+query, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.LogsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	return resp
}

// messages lists the response's entry messages in order.
func messages(resp adminapi.LogsResponse) []string {
	out := make([]string, len(resp.Entries))
	for i := range resp.Entries {
		out[i] = resp.Entries[i].Message
	}
	return out
}

// TestLogsHandler_ReturnsEntriesWithComponent asserts the endpoint maps buffer
// entries to the wire type and lifts the component attribute into its field.
func TestLogsHandler_ReturnsEntriesWithComponent(t *testing.T) {
	t.Parallel()
	resp := getLogs(t, fakeLogReader{entries: []telemetry.LogEntry{
		{Time: time.Now(), Level: "INFO", Message: "replicated", Attrs: map[string]any{"component": "replicator", "key": "foo"}},
		{Time: time.Now(), Level: "WARN", Message: "slow", Attrs: nil},
		{Time: time.Now(), Level: "INFO", Message: "tick", Attrs: map[string]any{"component": "scrubber"}},
	}}, "level=INFO&limit=50")

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

// TestLogsHandler_Filters asserts each query parameter reaches the buffer.
func TestLogsHandler_Filters(t *testing.T) {
	t.Parallel()
	now := time.Now()
	buf := telemetry.NewLogBuffer()
	buf.Add(telemetry.LogEntry{Time: now.Add(-time.Hour), Level: "DEBUG", Message: "old-debug", Attrs: map[string]any{"component": "server"}})
	buf.Add(telemetry.LogEntry{Time: now.Add(-time.Hour), Level: "WARN", Message: "old-warn", Attrs: map[string]any{"component": "storage"}})
	buf.Add(telemetry.LogEntry{Time: now, Level: "INFO", Message: "new-info", Attrs: map[string]any{"component": "server"}})
	buf.Add(telemetry.LogEntry{Time: now, Level: "ERROR", Message: "new-error", Attrs: map[string]any{"component": "storage"}})

	cutoff := now.Add(-10 * time.Minute).Format(time.RFC3339)
	for name, tc := range map[string]struct {
		query url.Values
		want  []string
	}{
		"default level hides debug":    {url.Values{}, []string{"old-warn", "new-info", "new-error"}},
		"debug shows all":              {url.Values{"level": {"DEBUG"}}, []string{"old-debug", "old-warn", "new-info", "new-error"}},
		"warn floor":                   {url.Values{"level": {"WARN"}}, []string{"old-warn", "new-error"}},
		"since":                        {url.Values{"since": {cutoff}}, []string{"new-info", "new-error"}},
		"before":                       {url.Values{"before": {cutoff}}, []string{"old-warn"}},
		"component":                    {url.Values{"level": {"DEBUG"}, "component": {"server"}}, []string{"old-debug", "new-info"}},
		"unparseable time is no bound": {url.Values{"since": {"yesterday"}}, []string{"old-warn", "new-info", "new-error"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := messages(getLogs(t, buf, tc.query.Encode()))
			if len(got) != len(tc.want) {
				t.Fatalf("messages = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("messages = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestLogsHandler_LimitKeepsNewest asserts a cut page keeps the newest entries
// and reports that older ones exist, and an uncut page does not.
func TestLogsHandler_LimitKeepsNewest(t *testing.T) {
	t.Parallel()
	buf := telemetry.NewLogBuffer()
	start := time.Now()
	for i := range 10 {
		buf.Add(telemetry.LogEntry{Time: start.Add(time.Duration(i) * time.Second), Level: "INFO", Message: string(rune('a' + i))})
	}

	cut := getLogs(t, buf, "limit=3")
	if got := messages(cut); len(got) != 3 || got[0] != "h" || got[2] != "j" || !cut.HasMore {
		t.Errorf("limit=3: messages = %v hasMore = %v, want [h i j] and true", got, cut.HasMore)
	}
	whole := getLogs(t, buf, "limit=20")
	if len(whole.Entries) != 10 || whole.HasMore {
		t.Errorf("limit=20: %d entries hasMore = %v, want 10 and false", len(whole.Entries), whole.HasMore)
	}
}

// TestLogsHandler_NoBufferReturns503 asserts the endpoint reports unavailable
// when no log buffer was wired.
func TestLogsHandler_NoBufferReturns503(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	LogsHandler(nil)(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/logs", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

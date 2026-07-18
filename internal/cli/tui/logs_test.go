// -------------------------------------------------------------------------------
// TUI - Logs View Tests
//
// Author: Alex Freidah
//
// Deterministic tests for the logs pane: the entry-to-row mapping, applying a
// loaded page (cursor parks on the newest row), key handling (back, reload),
// the load error path, and the pane-focus title styling.
// -------------------------------------------------------------------------------

package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

func TestRowsFromLogs(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 7, 18, 13, 5, 9, 0, time.UTC)
	rows := rowsFromLogs([]adminapi.LogEntry{
		{Time: ts, Level: "INFO", Component: "replicator", Message: "copied object",
			Attrs: map[string]any{"to": "e2", "from": "r2"}},
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	// Message column carries the message plus its attributes as sorted,
	// human-readable key=value pairs.
	if r := rows[0]; r[0] != "13:05:09" || r[1] != "INFO" || r[2] != "replicator" ||
		r[3] != "copied object from=r2 to=e2" {
		t.Errorf("row = %v", rows[0])
	}
}

func TestApplyLogs_CursorParksOnNewest(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.logs = logsView{loading: true, table: newTable()}
	m.resizeLogs()

	m.applyLogs(&adminapi.LogsResponse{Entries: []adminapi.LogEntry{
		{Level: "INFO", Message: "one"},
		{Level: "WARN", Message: "two"},
		{Level: "ERROR", Message: "three"},
	}})
	if m.logs.loading || m.logs.err != nil {
		t.Errorf("state = %+v", m.logs)
	}
	if len(m.logs.table.Rows()) != 3 {
		t.Fatalf("rows = %d, want 3", len(m.logs.table.Rows()))
	}
	if m.logs.table.Cursor() != 2 {
		t.Errorf("cursor = %d, want 2 (newest)", m.logs.table.Cursor())
	}
}

func TestHandleLogsKey_BackAndReload(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionLogs
	m.logs = logsView{table: newTable()}

	// esc returns focus to the sidebar, cursor on the current section.
	m.handleLogsKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !m.navFocus || m.navCursor != int(sectionLogs) {
		t.Errorf("after esc: navFocus=%v cursor=%d", m.navFocus, m.navCursor)
	}

	// "r" reloads: sets loading and returns a fetch command.
	m.navFocus = false
	_, cmd := m.handleLogsKey(key("r"))
	if !m.logs.loading || cmd == nil {
		t.Errorf("reload: loading=%v cmd=%v", m.logs.loading, cmd)
	}
	if _, ok := cmd().(logsLoadedMsg); !ok {
		t.Errorf("reload result = %#v, want logsLoadedMsg", cmd())
	}

	// a movement key delegates to the table without leaving the pane.
	m.handleLogsKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.navFocus {
		t.Error("movement key should not drop focus to the nav")
	}
}

func TestLogsBodyView_States(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.logs.err = errors.New("boom")
	if got := m.logsBodyView(); !strings.Contains(got, "boom") {
		t.Errorf("error body = %q", got)
	}
	m.logs = logsView{loading: true}
	if got := m.logsBodyView(); !strings.Contains(got, "loading") {
		t.Errorf("loading body = %q", got)
	}
	m.logs = logsView{}
	if got := m.logsBodyView(); !strings.Contains(got, "no log entries") {
		t.Errorf("empty body = %q", got)
	}
}

func TestLoadLogs_Error(t *testing.T) {
	t.Parallel()
	cmd := initialModel(errLister{}).loadLogs()
	if _, ok := cmd().(logsErrMsg); !ok {
		t.Errorf("cmd result = %#v, want logsErrMsg", cmd())
	}
}

func TestContentTitleStyle_FocusFlips(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})

	m.navFocus = false // content focused
	focused := m.contentTitleStyle().GetBackground()
	m.navFocus = true // nav focused -> content muted
	muted := m.contentTitleStyle().GetBackground()

	if focused == muted {
		t.Errorf("content title background should change with focus: both %v", focused)
	}
}

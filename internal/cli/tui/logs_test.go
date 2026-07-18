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

func TestLogLine(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 7, 18, 13, 5, 9, 0, time.UTC)
	line := logLine(&adminapi.LogEntry{
		Time: ts, Level: "INFO", Component: "replicator", Message: "copied object",
		Attrs: map[string]any{"to": "e2", "from": "r2"},
	}, 80)
	// The line carries time, level, component, and the message with its
	// attributes rendered as sorted key=value pairs.
	for _, want := range []string{"13:05:09", "INFO", "replicator", "copied object from=r2 to=e2"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q missing %q", line, want)
		}
	}
}

func TestApplyLogs_PopulatesViewport(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.logs = logsView{loading: true}
	m.resizeLogs()

	m.applyLogs(&adminapi.LogsResponse{Entries: []adminapi.LogEntry{
		{Level: "INFO", Message: "one"},
		{Level: "WARN", Message: "two"},
		{Level: "ERROR", Message: "three"},
	}})
	if m.logs.loading || m.logs.err != nil {
		t.Errorf("state = %+v", m.logs)
	}
	if len(m.logs.entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(m.logs.entries))
	}
	// The viewport shows the rendered lines.
	view := m.logs.vp.View()
	for _, want := range []string{"one", "two", "three", "WARN", "ERROR"} {
		if !strings.Contains(view, want) {
			t.Errorf("viewport missing %q:\n%s", want, view)
		}
	}
}

func TestHandleLogsKey_BackAndReload(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.section = sectionLogs
	m.logs = logsView{}
	m.resizeLogs()

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

	// a movement key delegates to the viewport without leaving the pane.
	m.handleLogsKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.navFocus {
		t.Error("movement key should not drop focus to the nav")
	}
}

func TestLogsHeaderView_ShowsLevelFilter(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20

	if got := m.logsHeaderView(); !strings.Contains(got, "level: all") {
		t.Errorf("default header should show level: all, got %q", got)
	}
	m.logs.minLevel = "WARN"
	if got := m.logsHeaderView(); !strings.Contains(got, "level: WARN+") {
		t.Errorf("filtered header should show level: WARN+, got %q", got)
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

func TestNextLogLevel(t *testing.T) {
	t.Parallel()
	cases := map[string]string{"": "INFO", "INFO": "WARN", "WARN": "ERROR", "ERROR": "", "bogus": ""}
	for in, want := range cases {
		if got := nextLogLevel(in); got != want {
			t.Errorf("nextLogLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandleLogsKey_CyclesLevel(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionLogs
	m.logs = logsView{}

	// "L" advances the level floor and triggers a re-fetch.
	_, cmd := m.handleLogsKey(key("L"))
	if m.logs.minLevel != "INFO" {
		t.Errorf("minLevel = %q, want INFO", m.logs.minLevel)
	}
	if !m.logs.loading || cmd == nil {
		t.Errorf("cycle: loading=%v cmd=%v", m.logs.loading, cmd)
	}
	if _, ok := cmd().(logsLoadedMsg); !ok {
		t.Errorf("cycle result = %#v, want logsLoadedMsg", cmd())
	}
}

func TestLevelStyle_Normalizes(t *testing.T) {
	t.Parallel()
	// case-insensitive and WARNING alias map to the same style; unknown -> info.
	if levelStyle("warn").GetForeground() != levelStyle("WARNING").GetForeground() {
		t.Error("warn and WARNING should share a style")
	}
	if levelStyle("weird").GetForeground() != levelStyle("INFO").GetForeground() {
		t.Error("unknown level should fall back to the info style")
	}
	if levelStyle("ERROR").GetForeground() == levelStyle("INFO").GetForeground() {
		t.Error("error and info styles should differ")
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

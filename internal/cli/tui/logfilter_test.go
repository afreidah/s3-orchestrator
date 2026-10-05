// -------------------------------------------------------------------------------
// TUI - Log Filter Tests
//
// Author: Alex Freidah
//
// Covers narrowing the Logs pane to the entries whose component or message
// contains the typed text: matching as it is typed, keeping the filter on
// enter, clearing it on esc before esc leaves the pane, and the notices for a
// filter that matches nothing.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	tea "github.com/charmbracelet/bubbletea"
)

// logsWithEntries returns a model on the Logs pane with three entries loaded:
// two from the replicator, one of which names a key only in its attributes.
func logsWithEntries(t *testing.T) *model {
	t.Helper()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.section = sectionLogs
	m.applyLogs(&adminapi.LogsResponse{Entries: []adminapi.LogEntry{
		{Level: "INFO", Component: "replicator", Message: "object replicated", Attrs: map[string]any{"key": "photos/a.jpg"}},
		{Level: "WARN", Component: "scrubber", Message: "copy mismatch"},
		{Level: "INFO", Component: "replicator", Message: "replication cycle complete"},
	}})
	return m
}

// typeText sends each rune of s as a key press.
func typeText(m *model, s string) {
	for _, r := range s {
		m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// TestLogsFilter_NarrowsAsYouType verifies "/" starts a filter that matches
// the component and the message with its attributes, and the header counts
// what it lets through.
func TestLogsFilter_NarrowsAsYouType(t *testing.T) {
	t.Parallel()
	m := logsWithEntries(t)
	m.handleKey(key("/"))
	if !m.logs.filtering {
		t.Fatal("/ did not start the filter")
	}

	typeText(m, "replicator")
	if got := len(m.visibleLogs()); got != 2 {
		t.Errorf("component filter kept %d entries, want 2", got)
	}
	if header := m.logsHeaderView(); !strings.Contains(header, "2 of 3 entries") {
		t.Errorf("header = %q, want the filtered count", header)
	}

	m.clearLogsFilter()
	m.handleKey(key("/"))
	typeText(m, "PHOTOS/A")
	view := m.contentView()
	if !strings.Contains(view, "object replicated") || strings.Contains(view, "copy mismatch") {
		t.Errorf("an attribute match should keep only its entry:\n%s", view)
	}
}

// TestLogsFilter_EnterKeepsEscClears verifies enter keeps the filter applied
// and hands keys back to the pane, the first esc clears it, and the next esc
// leaves for the nav.
func TestLogsFilter_EnterKeepsEscClears(t *testing.T) {
	t.Parallel()
	m := logsWithEntries(t)
	m.handleKey(key("/"))
	typeText(m, "scrub")
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.logs.filtering || len(m.visibleLogs()) != 1 {
		t.Fatalf("after enter: filtering=%v visible=%d, want the filter kept and keys back", m.logs.filtering, len(m.visibleLogs()))
	}
	if header := m.logsHeaderView(); !strings.Contains(header, "filter: scrub") {
		t.Errorf("header = %q, want the applied filter", header)
	}

	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.logs.filter.Value() != "" || m.navFocus {
		t.Errorf("first esc: filter=%q navFocus=%v, want the filter cleared and the pane kept", m.logs.filter.Value(), m.navFocus)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !m.navFocus {
		t.Error("second esc should return to the nav")
	}
}

// TestLogsFilter_EscWhileTypingAbandons verifies esc while typing drops the
// filter and shows every entry.
func TestLogsFilter_EscWhileTypingAbandons(t *testing.T) {
	t.Parallel()
	m := logsWithEntries(t)
	m.handleKey(key("/"))
	typeText(m, "scrub")
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.logs.filtering || m.logs.filter.Value() != "" || len(m.visibleLogs()) != 3 {
		t.Errorf("esc while typing: filtering=%v value=%q visible=%d", m.logs.filtering, m.logs.filter.Value(), len(m.visibleLogs()))
	}
}

// TestLogsFilter_NoMatches verifies a filter that matches nothing says so
// rather than showing an empty pane.
func TestLogsFilter_NoMatches(t *testing.T) {
	t.Parallel()
	m := logsWithEntries(t)
	m.handleKey(key("/"))
	typeText(m, "nothing-like-this")
	if got := bodyText(m.logsBody()); !strings.Contains(got, "no matches") {
		t.Errorf("body = %q, want the no-matches notice", got)
	}
}

// -------------------------------------------------------------------------------
// TUI - Logs View
//
// Author: Alex Freidah
//
// Read-only pane over the instance's in-memory structured-log ring buffer,
// fetched from the admin logs endpoint (the same source the web dashboard's
// logs pane reads). Renders recent entries oldest-first as time / level /
// component / message, colouring the level by severity. The minimum-level
// filter cycles with "L"; "r" refreshes. Lines are rendered by hand into a
// scrolling viewport rather than a table because the table truncates cells by
// counting ANSI colour codes toward the column width.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// Column widths for the logs table (message takes the remainder).
const (
	logTimeWidth      = 8
	logLevelWidth     = 5
	logComponentWidth = 16
	// logFixedWidth is the space the non-message columns and their single-space
	// separators consume, subtracted from the pane width to size the message.
	logFixedWidth = logTimeWidth + 1 + logLevelWidth + 1 + logComponentWidth + 1
)

// logsView holds the state of the logs pane.
type logsView struct {
	entries  []adminapi.LogEntry // recent entries, oldest first
	vp       viewport.Model      // scrolling viewport over the rendered lines
	minLevel string              // minimum severity filter ("" = all levels)
	loading  bool                // a logs fetch is in flight
	err      error               // last fetch error, if any
}

// logLevelCycle is the order the level filter steps through: all levels, then
// each floor in ascending severity.
var logLevelCycle = []string{"", "INFO", "WARN", "ERROR"}

// nextLogLevel returns the level filter after current in the cycle.
func nextLogLevel(current string) string {
	for i, l := range logLevelCycle {
		if l == current {
			return logLevelCycle[(i+1)%len(logLevelCycle)]
		}
	}
	return ""
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// logsLoadedMsg carries a successfully loaded log page.
type logsLoadedMsg struct{ resp *adminapi.LogsResponse }

// logsErrMsg carries a failed logs fetch.
type logsErrMsg struct{ err error }

// loadLogs returns a command that fetches recent log entries off the main loop
// at the current level floor, delivering a logsLoadedMsg or logsErrMsg.
func (m *model) loadLogs() tea.Cmd {
	client := m.client
	level := m.logs.minLevel
	return func() tea.Msg {
		resp, err := client.GetLogs(context.Background(), level)
		if err != nil {
			return logsErrMsg{err}
		}
		return logsLoadedMsg{resp}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyLogs folds a loaded page into the logs state and scrolls to the bottom
// so the freshest activity is in view.
func (m *model) applyLogs(resp *adminapi.LogsResponse) {
	m.logs.entries = resp.Entries
	m.logs.vp.SetContent(m.renderLogLines())
	m.logs.vp.GotoBottom()
	m.logs.loading = false
	m.logs.err = nil
}

// handleLogsKey applies logs-level keys (back, reload, level filter) and
// delegates scrolling to the viewport.
func (m *model) handleLogsKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "left", "h":
		m.navFocus = true
		m.navCursor = int(m.section)
		return m, nil
	case "r":
		m.logs.loading = true
		cmd := m.loadLogs()
		return m, cmd
	case "L":
		// cycle the minimum-level filter and re-fetch at the new floor.
		m.logs.minLevel = nextLogLevel(m.logs.minLevel)
		m.logs.loading = true
		cmd := m.loadLogs()
		return m, cmd
	}

	var cmd tea.Cmd
	m.logs.vp, cmd = m.logs.vp.Update(key)
	return m, cmd
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// resizeLogs fits the logs viewport to the window (below the two-line header
// and one-line footer) and re-renders its content at the new width.
func (m *model) resizeLogs() {
	m.logs.vp.Width = m.contentWidth()
	m.logs.vp.Height = max(m.height-3, 3)
	m.logs.vp.SetContent(m.renderLogLines())
}

// msgWidth is the width left for the message column after the fixed columns.
func (m *model) msgWidth() int {
	return max(m.contentWidth()-logFixedWidth, 10)
}

// renderLogLines formats every entry into a fixed-width, level-coloured line,
// joined newest-last.
func (m *model) renderLogLines() string {
	msgW := m.msgWidth()
	lines := make([]string, len(m.logs.entries))
	for i := range m.logs.entries {
		lines[i] = logLine(&m.logs.entries[i], msgW)
	}
	return strings.Join(lines, "\n")
}

// logLine renders one entry: time, the severity-coloured level, component, and
// the message with its attributes. Each field is padded/truncated in plaintext
// first so the colour codes on the level never shift the columns.
func logLine(e *adminapi.LogEntry, msgW int) string {
	level := levelStyle(e.Level).Render(fmt.Sprintf("%-*s", logLevelWidth, truncate(e.Level, logLevelWidth)))
	return fmt.Sprintf("%-*s %s %-*s %s",
		logTimeWidth, e.Time.Format("15:04:05"),
		level,
		logComponentWidth, truncate(e.Component, logComponentWidth),
		truncate(logMessage(e), msgW),
	)
}

// logMessage renders a full, human-readable log line: the message followed by
// its structured attributes as space-separated key=value pairs (sorted for
// stable output). This is where the detail lives - the bare Message is often a
// terse stub like "object replicated".
func logMessage(e *adminapi.LogEntry) string {
	if len(e.Attrs) == 0 {
		return e.Message
	}
	keys := make([]string, 0, len(e.Attrs))
	for k := range e.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(e.Message)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, e.Attrs[k])
	}
	return b.String()
}

// logsPaneView composes the pane's full-screen layout.
func (m *model) logsPaneView() string {
	return m.frame(m.logsHeaderView(), m.logsFooterView(), m.logsBodyView())
}

// logsHeaderView renders the title bar (entry count + level filter) plus the
// column header row beneath it.
func (m *model) logsHeaderView() string {
	level := "all"
	if m.logs.minLevel != "" {
		level = m.logs.minLevel + "+"
	}
	title := fmt.Sprintf("logs   %d entries   level: %s", len(m.logs.entries), level)
	cols := fmt.Sprintf("%-*s %-*s %-*s %s",
		logTimeWidth, "TIME", logLevelWidth, "LEVEL", logComponentWidth, "COMPONENT", "MESSAGE")
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title) + "\n" +
		colHeaderStyle.Width(m.contentWidth()).Render(cols)
}

// logsFooterView renders the logs key hints.
func (m *model) logsFooterView() string {
	return m.footer("up/down move - L level - r reload - tab nav - q quit")
}

// logsBodyView renders the current content: an error, the loading indicator, an
// empty notice, or the scrolling log viewport.
func (m *model) logsBodyView() string {
	switch {
	case m.logs.err != nil:
		return errStyle.Render("error: " + m.logs.err.Error())
	case m.logs.loading:
		return m.spinner.View() + " loading..."
	case len(m.logs.entries) == 0:
		return pathStyle.Render("(no log entries)")
	default:
		return m.logs.vp.View()
	}
}

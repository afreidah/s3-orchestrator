// -------------------------------------------------------------------------------
// TUI - Logs View
//
// Author: Alex Freidah
//
// Read-only pane over the instance's in-memory structured-log ring buffer,
// fetched from the admin logs endpoint (the same source the web dashboard's
// logs pane reads). Renders recent entries oldest-first as time / level /
// component / message, colouring the level by severity. The minimum-level
// filter cycles with "L" and re-fetches; "/" narrows the loaded entries to
// those whose component or message contains the typed text; "r" refreshes;
// "F" follows, re-fetching on the shared poller and holding the view on the
// newest entries unless the operator has scrolled up. Lines are rendered by
// hand into a scrolling viewport rather than a table because the table truncates cells by
// counting ANSI colour codes toward the column width.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// Column widths for the logs table (message takes the remainder).
const (
	logTimeWidth      = 8
	logLevelWidth     = 5
	logComponentWidth = 16
	logFixedWidth     = logTimeWidth + 1 + logLevelWidth + 1 + logComponentWidth + 1 // columns plus separators
)

// logsView holds the state of the logs pane.
type logsView struct {
	entries   []adminapi.LogEntry // recent entries, oldest first
	vp        viewport.Model      // scrolling viewport over the rendered lines
	minLevel  string              // minimum severity filter ("" = all levels)
	filter    textinput.Model     // substring filter over the component and message
	filtering bool                // the filter input has focus and is capturing keys
	following bool                // the poller re-fetches the entries while the pane shows
	loading   bool                // a logs fetch is in flight
	err       error               // last fetch error, if any
}

// newLogsView builds the pane's empty state.
func newLogsView() logsView {
	return logsView{filter: newFilterInput()}
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

// logsLoadedMsg carries a successfully loaded log page and the level floor it
// was fetched at.
type logsLoadedMsg struct {
	resp  *adminapi.LogsResponse
	level string
}

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
		return logsLoadedMsg{resp: resp, level: level}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyLogs folds a loaded page into the logs state. A page fetched at a level
// floor the operator has since moved off is dropped. The view stays on the
// newest entries unless the operator has scrolled up to read older ones.
func (m *model) applyLogs(msg logsLoadedMsg) {
	if msg.level != m.logs.minLevel {
		return
	}
	atBottom := m.logs.vp.AtBottom()
	m.logs.entries = msg.resp.Entries
	m.logs.vp.SetContent(m.renderLogLines())
	if atBottom {
		m.logs.vp.GotoBottom()
	}
	m.logs.loading = false
	m.logs.err = nil
}

// refreshLogLines re-renders the entries the filter lets through and scrolls
// to the newest.
func (m *model) refreshLogLines() {
	m.logs.vp.SetContent(m.renderLogLines())
	m.logs.vp.GotoBottom()
}

// handleLogsKey applies logs-level keys (filter, back, reload, level filter)
// and delegates scrolling to the viewport. esc clears an applied filter
// before it leaves the pane, as it does in Files.
func (m *model) handleLogsKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "/":
		m.logs.filtering = true
		cmd := m.logs.filter.Focus()
		return m, cmd
	case "esc":
		if m.logs.filter.Value() != "" {
			m.clearLogsFilter()
			return m, nil
		}
		return m.navBack()
	case "left", "h":
		return m.navBack()
	case "r":
		cmd := m.fetch(pollLogs)
		return m, cmd
	case "F":
		m.logs.following = !m.logs.following
		return m, nil
	case "L":
		// cycle the minimum-level filter and re-fetch at the new floor, even
		// past a request in flight, whose page applyLogs will now drop.
		m.logs.minLevel = nextLogLevel(m.logs.minLevel)
		m.logs.loading = true
		cmd := m.fetchAt(pollLogs, time.Now())
		return m, cmd
	}

	var cmd tea.Cmd
	m.logs.vp, cmd = m.logs.vp.Update(key)
	return m, cmd
}

// handleLogsFilterKey feeds keys to the focused filter. esc abandons the
// filter, enter keeps it applied and returns the keys to the pane, and every
// other key edits the filter and narrows the lines as it is typed.
func (m *model) handleLogsFilterKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc":
		m.clearLogsFilter()
		return m, nil
	case "enter":
		m.logs.filtering = false
		m.logs.filter.Blur()
		return m, nil
	}

	var cmd tea.Cmd
	m.logs.filter, cmd = m.logs.filter.Update(key)
	m.refreshLogLines()
	return m, cmd
}

// clearLogsFilter empties and blurs the filter and shows every entry again.
func (m *model) clearLogsFilter() {
	m.logs.filtering = false
	m.logs.filter.Blur()
	m.logs.filter.SetValue("")
	m.refreshLogLines()
}

// visibleLogs returns the entries the filter lets through: those whose
// component or full message, attributes included, contains the filter text,
// ignoring case.
func (m *model) visibleLogs() []adminapi.LogEntry {
	needle := strings.ToLower(m.logs.filter.Value())
	if needle == "" {
		return m.logs.entries
	}
	var out []adminapi.LogEntry
	for i := range m.logs.entries {
		e := &m.logs.entries[i]
		if strings.Contains(strings.ToLower(e.Component), needle) ||
			strings.Contains(strings.ToLower(logMessage(e)), needle) {
			out = append(out, *e)
		}
	}
	return out
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// resizeLogs re-renders the log lines at the new width, since each message is
// truncated to the width left after the fixed columns. The viewport itself is
// sized by its pane.
func (m *model) resizeLogs() {
	m.logs.vp.SetContent(m.renderLogLines())
}

// msgWidth is the width left for the message column after the fixed columns.
func (m *model) msgWidth() int {
	return max(m.contentWidth()-logFixedWidth, 10)
}

// renderLogLines formats every entry the filter lets through into a
// fixed-width, level-coloured line, joined newest-last.
func (m *model) renderLogLines() string {
	msgW := m.msgWidth()
	visible := m.visibleLogs()
	lines := make([]string, len(visible))
	for i := range visible {
		lines[i] = logLine(&visible[i], msgW)
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
	keys := slices.Sorted(maps.Keys(e.Attrs))

	var b strings.Builder
	b.WriteString(e.Message)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, e.Attrs[k])
	}
	return b.String()
}

// logsPaneView composes the pane's full-screen layout.
func (m *model) logsPaneView() string {
	return m.frame(m.logsHeaderView(), m.hintFooter(), m.logsBody()...)
}

// logsHeaderView renders the title bar (entry count, level filter, whether the
// pane is following, and the text filter while one is being typed or applied)
// plus the column header row beneath it.
func (m *model) logsHeaderView() string {
	level := "all"
	if m.logs.minLevel != "" {
		level = m.logs.minLevel + "+"
	}
	name := "logs"
	if m.logs.following {
		name = "logs (following)"
	}
	title := fmt.Sprintf("%s   %d entries   level: %s", name, len(m.logs.entries), level)
	switch {
	case m.logs.filtering:
		title = fmt.Sprintf("%s   %d of %d entries   level: %s   filter: %s",
			name, len(m.visibleLogs()), len(m.logs.entries), level, m.logs.filter.View())
	case m.logs.filter.Value() != "":
		title = fmt.Sprintf("%s   %d of %d entries   level: %s   filter: %s",
			name, len(m.visibleLogs()), len(m.logs.entries), level, m.logs.filter.Value())
	}
	cols := fmt.Sprintf("%-*s %-*s %-*s %s",
		logTimeWidth, "TIME", logLevelWidth, "LEVEL", logComponentWidth, "COMPONENT", "MESSAGE")
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title) + "\n" +
		colHeaderStyle.Width(m.contentWidth()).Render(cols)
}

// logsBody renders the current content: an error, the loading indicator, an
// empty or no-matches notice, or the scrolling log viewport.
func (m *model) logsBody() []pane {
	return m.paneBody(m.logs.err, "", m.logs.loading, func() []pane {
		switch {
		case len(m.logs.entries) == 0:
			return []pane{textPane(pathStyle.Render("(no log entries)"))}
		case len(m.visibleLogs()) == 0:
			return []pane{textPane(pathStyle.Render("(no matches)"))}
		default:
			return []pane{m.viewportPane(&m.logs.vp)}
		}
	})
}

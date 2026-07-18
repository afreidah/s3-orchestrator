// -------------------------------------------------------------------------------
// TUI - Logs View
//
// Author: Alex Freidah
//
// Read-only pane over the instance's in-memory structured-log ring buffer,
// fetched from the admin logs endpoint (the same source the web dashboard's
// logs pane reads). Renders recent entries oldest-first as time / level /
// component / message. Reached from the Logs nav section; "r" refreshes.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// logsView holds the state of the logs pane.
type logsView struct {
	entries []adminapi.LogEntry // recent entries, oldest first
	table   table.Model         // scrolling table over the entries
	loading bool                // a logs fetch is in flight
	err     error               // last fetch error, if any
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// logsLoadedMsg carries a successfully loaded log page.
type logsLoadedMsg struct{ resp *adminapi.LogsResponse }

// logsErrMsg carries a failed logs fetch.
type logsErrMsg struct{ err error }

// loadLogs returns a command that fetches recent log entries off the main loop,
// delivering the result back as a logsLoadedMsg or logsErrMsg.
func (m *model) loadLogs() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.GetLogs(context.Background())
		if err != nil {
			return logsErrMsg{err}
		}
		return logsLoadedMsg{resp}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyLogs folds a loaded page into the logs state, parking the cursor on the
// newest (last) row so the freshest activity is in view.
func (m *model) applyLogs(resp *adminapi.LogsResponse) {
	m.logs.entries = resp.Entries
	m.logs.table.SetRows(rowsFromLogs(resp.Entries))
	m.logs.table.SetCursor(max(len(resp.Entries)-1, 0))
	m.logs.loading = false
	m.logs.err = nil
}

// handleLogsKey applies logs-level keys (back, reload) and delegates cursor
// movement to the table.
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
	}

	var cmd tea.Cmd
	m.logs.table, cmd = m.logs.table.Update(key)
	return m, cmd
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// resizeLogs fits the logs columns and viewport to the window, giving the
// message column whatever width the fixed columns leave behind.
func (m *model) resizeLogs() {
	const (
		fixed = 8 + 7 + 16 // time + level + component
		cols  = 4
	)
	msgWidth := max(m.contentWidth()-fixed-cols*tableCellPad, 20)
	m.logs.table.SetColumns([]table.Column{
		{Title: "TIME", Width: 8},
		{Title: "LEVEL", Width: 7},
		{Title: "COMPONENT", Width: 16},
		{Title: "MESSAGE", Width: msgWidth},
	})
	m.logs.table.SetWidth(m.contentWidth())
	m.logs.table.SetHeight(max(m.height-2, 3))
}

// rowsFromLogs builds table rows from the log entries, in the same order so the
// table cursor indexes straight into entries.
func rowsFromLogs(entries []adminapi.LogEntry) []table.Row {
	rows := make([]table.Row, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		rows = append(rows, table.Row{
			e.Time.Format("15:04:05"),
			e.Level,
			e.Component,
			logMessage(e),
		})
	}
	return rows
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

// logsHeaderView renders the title bar with the entry count.
func (m *model) logsHeaderView() string {
	title := fmt.Sprintf("logs   %d entries", len(m.logs.entries))
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title)
}

// logsFooterView renders the logs key hints.
func (m *model) logsFooterView() string {
	return helpStyle.Width(m.contentWidth()).Render("up/down move - tab nav - r reload - q quit")
}

// logsBodyView renders the current content: an error, the loading indicator, an
// empty notice, or the log table.
func (m *model) logsBodyView() string {
	switch {
	case m.logs.err != nil:
		return errStyle.Render("error: " + m.logs.err.Error())
	case m.logs.loading:
		return m.spinner.View() + " loading..."
	case len(m.logs.entries) == 0:
		return pathStyle.Render("(no log entries)")
	default:
		return m.logs.table.View()
	}
}

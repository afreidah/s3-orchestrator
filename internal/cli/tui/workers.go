// -------------------------------------------------------------------------------
// TUI - Workers View
//
// Author: Alex Freidah
//
// Read-only pane over the background services' last-tick health. A worker that
// is running but failing every tick is indistinguishable from a healthy one in
// /health, so this pane exists to make that difference visible: the failure
// count and last error sit beside the last success time. Reached with "w";
// "esc" returns focus to the nav, "r" reloads.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// workersView holds the state of the worker health pane.
type workersView struct {
	rows        []adminapi.WorkerHealth // one entry per registered background service
	table       table.Model             // scrolling table over the workers
	loading     bool                    // a fetch is in flight
	unavailable string                  // set when the deployment registers no workers
	err         error                   // last fetch error, if any
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// workersLoadedMsg carries a successfully loaded worker health snapshot.
type workersLoadedMsg struct{ resp *adminapi.WorkersResponse }

// workersErrMsg carries a failed worker health fetch.
type workersErrMsg struct{ err error }

// loadWorkers returns a command that fetches worker health off the main loop.
func (m *model) loadWorkers() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.GetWorkers(context.Background())
		if err != nil {
			return workersErrMsg{err}
		}
		return workersLoadedMsg{resp}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyWorkers folds a loaded snapshot into the pane state.
func (m *model) applyWorkers(resp *adminapi.WorkersResponse) {
	m.workers.rows = resp.Workers
	m.workers.table.SetRows(rowsFromWorkers(resp.Workers))
	m.workers.table.SetCursor(0)
	m.workers.loading = false
	m.workers.unavailable = ""
	m.workers.err = nil
}

// applyWorkersErr records a failed fetch, separating a proxy-only deployment
// (which registers no worker pool) from a real failure.
func (m *model) applyWorkersErr(err error) {
	m.workers.loading = false
	m.workers.unavailable = unavailableReason(err)
	m.workers.err = nil
	if m.workers.unavailable == "" {
		m.workers.err = err
	}
}

// handleWorkersKey applies pane keys (back, reload) and delegates cursor
// movement to the table.
func (m *model) handleWorkersKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "left", "h":
		m.navFocus = true
		m.navCursor = int(m.section)
		return m, nil
	case "r":
		m.workers.loading = true
		cmd := m.loadWorkers()
		return m, cmd
	}

	var cmd tea.Cmd
	m.workers.table, cmd = m.workers.table.Update(key)
	return m, cmd
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// resizeWorkers fits the workers columns and viewport to the window.
func (m *model) resizeWorkers() {
	const (
		fixed   = 12 + 12 + 7 // last ok, last fail, fails
		cols    = 5
		nameCap = 28
	)
	nameWidth := fitFirstColumn(m.contentWidth(), fixed, cols, nameCap)
	errWidth := max(m.contentWidth()-nameWidth-fixed-cols*tableCellPad, 8)
	m.workers.table.SetColumns([]table.Column{
		{Title: "WORKER", Width: nameWidth},
		{Title: "LAST OK", Width: 12},
		{Title: "LAST FAIL", Width: 12},
		{Title: "FAILS", Width: 7},
		{Title: "LAST ERROR", Width: errWidth},
	})
	m.workers.table.SetWidth(m.contentWidth())
	m.workers.table.SetHeight(max(m.height-3, 3))
}

// rowsFromWorkers builds table rows from the health snapshot, in the same order
// so the table cursor indexes straight into rows.
func rowsFromWorkers(workers []adminapi.WorkerHealth) []table.Row {
	rows := make([]table.Row, 0, len(workers))
	for i := range workers {
		w := workers[i]
		rows = append(rows, table.Row{
			w.Name,
			tickAge(w.LastSuccess),
			tickAge(w.LastFailure),
			strconv.Itoa(w.ConsecutiveFailures),
			w.LastError,
		})
	}
	return rows
}

// tickAge renders how long ago a tick outcome was recorded. A zero time means
// the worker has never recorded that outcome, which is normal for a service
// that has only ever succeeded.
func tickAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return humanDuration(max(time.Since(t), 0)) + " ago"
}

// workersPaneView composes the pane's full-screen layout.
func (m *model) workersPaneView() string {
	return m.frame(m.workersHeaderView(), m.workersFooterView(), m.workersBodyView())
}

// workersHeaderView renders the title bar with the worker count and how many
// are currently failing.
func (m *model) workersHeaderView() string {
	title := fmt.Sprintf("workers   %d registered", len(m.workers.rows))
	if failing := failingWorkers(m.workers.rows); failing > 0 {
		title += fmt.Sprintf("   %d failing", failing)
	}
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title)
}

// failingWorkers counts services whose most recent tick failed.
func failingWorkers(workers []adminapi.WorkerHealth) int {
	n := 0
	for i := range workers {
		if workers[i].ConsecutiveFailures > 0 {
			n++
		}
	}
	return n
}

// workersFooterView renders the workers key hints.
func (m *model) workersFooterView() string {
	return m.footer("up/down move - r reload - tab nav - q quit")
}

// workersBodyView renders the current content: an error, a not-wired notice,
// the loading indicator, or the workers table.
func (m *model) workersBodyView() string {
	switch {
	case m.workers.err != nil:
		return errStyle.Render("error: " + m.workers.err.Error())
	case m.workers.unavailable != "":
		return pathStyle.Render("(" + m.workers.unavailable + ")")
	case m.workers.loading:
		return m.spinner.View() + " loading..."
	case len(m.workers.rows) == 0:
		return pathStyle.Render("(no workers registered)")
	default:
		return m.workers.table.View()
	}
}

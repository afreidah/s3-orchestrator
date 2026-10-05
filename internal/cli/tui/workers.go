// -------------------------------------------------------------------------------
// TUI - Workers View
//
// Author: Alex Freidah
//
// Pane over the background services' last-tick health. A worker that is
// running but failing every tick is indistinguishable from a healthy one in
// /health, so this pane exists to make that difference visible: the failure
// count and last error sit beside the last success time. "R" runs the
// highlighted worker now and streams the run into the ops output pane.
// Reached with "w"; "esc" returns focus to the nav, "r" reloads.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// workersView holds the state of the worker health pane.
type workersView struct {
	list        sortTable[adminapi.WorkerHealth] // one entry per registered background service
	loading     bool                             // a fetch is in flight
	unavailable string                           // set when the deployment registers no workers
	err         error                            // last fetch error, if any
}

// newWorkersView builds the pane's empty state.
func newWorkersView() workersView {
	return workersView{list: newSortTable(workerColumns, workerSorts, rowsFromWorkers,
		func(w *adminapi.WorkerHealth) string { return w.Name })}
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
	m.workers.list.setItems(resp.Workers)
	m.workers.loading = false
	m.workers.unavailable = ""
	m.workers.err = nil
}

// applyWorkersErr records a failed fetch, separating a proxy-only deployment
// (which registers no worker pool) from a real failure.
func (m *model) applyWorkersErr(err error) {
	m.workers.loading = false
	m.workers.unavailable = adminclient.UnavailableReason(err)
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
		return m.navBack()
	case "r":
		m.workers.loading = !m.workers.list.loaded()
		cmd := m.fetch(pollWorkers)
		return m, cmd
	case "R":
		return m.armRunWorker()
	}

	cmd := m.workers.list.update(key)
	return m, cmd
}

// armRunWorker confirms running the highlighted worker now, then streams the
// run into the ops output pane: each line the worker logs, then the outcome.
// esc from there returns here, where the next refresh shows the tick in the
// worker's health.
func (m *model) armRunWorker() (tea.Model, tea.Cmd) {
	w, ok := m.workers.list.selected()
	if !ok {
		return m, nil
	}
	name := w.Name
	act := &opsAction{label: "run " + name, method: http.MethodPost, path: "/admin/api/workers/" + url.PathEscape(name) + "/run"}
	return m.startAction(adminAction{
		confirm: "Run the " + name + " worker now?",
		before: func(m *model) {
			m.section = sectionOps
			m.navFocus = false
			m.ops = opsView{worker: name}
			m.enterOpsOutput(act.label)
		},
		run: openOps(m.client, act, opsRequest{path: act.path}),
	})
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// workerColumns declares the workers table's columns. The name is capped and
// the last error takes the rest. A narrow terminal keeps which workers are
// failing and why, and drops the tick times first.
var workerColumns = []columnSpec{
	{title: "WORKER", min: 8, max: 28, priority: 5},
	{title: "LAST OK", min: 12, max: 12, priority: 2},
	{title: "LAST FAIL", min: 12, max: 12, priority: 1},
	{title: "FAILS", min: 7, max: 7, priority: 4},
	{title: "LAST ERROR", min: 8, max: 0, priority: 3},
}

// workerSorts orders the workers table by every column. The tick times sort
// by when they happened, so a worker that never recorded one sorts first.
var workerSorts = map[string]func(a, b *adminapi.WorkerHealth) int{
	"WORKER":     by(func(w *adminapi.WorkerHealth) string { return w.Name }),
	"LAST OK":    by(func(w *adminapi.WorkerHealth) int64 { return w.LastSuccess.UnixNano() }),
	"LAST FAIL":  by(func(w *adminapi.WorkerHealth) int64 { return w.LastFailure.UnixNano() }),
	"FAILS":      by(func(w *adminapi.WorkerHealth) int { return w.ConsecutiveFailures }),
	"LAST ERROR": by(func(w *adminapi.WorkerHealth) string { return w.LastError }),
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
	return m.frame(m.workersHeaderView(), m.hintFooter(), m.workersBody()...)
}

// workersHeaderView renders the title bar with the worker count and how many
// are currently failing.
func (m *model) workersHeaderView() string {
	title := fmt.Sprintf("workers   %d registered", len(m.workers.list.received))
	if failing := failingWorkers(m.workers.list.received); failing > 0 {
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

// workersBody renders the current content: an error, a not-wired notice, the
// loading indicator, or the workers table.
func (m *model) workersBody() []pane {
	return m.paneBody(m.workers.err, m.workers.unavailable, m.workers.loading, func() []pane {
		if len(m.workers.list.received) == 0 {
			return []pane{textPane(pathStyle.Render("(no workers registered)"))}
		}
		return []pane{m.workers.list.pane(m)}
	})
}

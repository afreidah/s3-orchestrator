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
	"fmt"
	"strconv"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// workersView holds the state of the worker health pane.
type workersView struct {
	loadState
	list sortTable[adminapi.WorkerHealth] // one entry per registered background service
}

// newWorkersView builds the pane's empty state.
func newWorkersView() workersView {
	return workersView{list: newSortTable(workerColumns, workerSorts, workerRow,
		func(w *adminapi.WorkerHealth) string { return w.Name })}
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// loadWorkers returns a command that fetches worker health off the main loop.
func (m *model) loadWorkers() tea.Cmd {
	return fetchCmd(pollWorkers, m.client.GetWorkers, (*model).applyWorkers,
		func(m *model, err error) { m.workers.failed(err) })
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyWorkers folds a loaded snapshot into the pane state.
func (m *model) applyWorkers(resp *adminapi.WorkersResponse) {
	m.workers.list.setItems(resp.Workers)
	m.workers.loaded()
}

// handleWorkersKey runs the highlighted worker's Ops action on "R" and
// delegates cursor movement to the table.
func (m *model) handleWorkersKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.String() == "R" {
		return m.runWorkerOpsAction()
	}

	cmd := m.workers.list.update(key)
	return m, cmd
}

// workerOpsPaths names the Ops action that does each worker's job, by the
// path the action posts to. A worker missing here has no Ops action.
var workerOpsPaths = map[string]string{
	"rebalance":        adminapi.PathRebalance,
	"replication":      adminapi.PathReplicate,
	"over_replication": adminapi.PathOverReplication,
	"scrub":            adminapi.PathScrub,
	"reconcile":        adminapi.PathReconcile,
	"lifecycle":        adminapi.PathLifecycle,
}

// runWorkerOpsAction runs the Ops action that does the highlighted worker's
// job, confirming and streaming it in this pane.
func (m *model) runWorkerOpsAction() (tea.Model, tea.Cmd) {
	w, ok := m.workers.list.selected()
	if !ok {
		return m, nil
	}
	path, ok := workerOpsPaths[w.Name]
	if !ok {
		return m, nil
	}
	if a, ok := actionAt(maintenanceActions(), path); ok {
		return m.startRun(a, opsRequest{path: path})
	}
	return m, nil
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

// workerRow renders one worker's health. A zero time is an outcome the worker
// has never recorded, which is normal for a service that has only succeeded.
func workerRow(w *adminapi.WorkerHealth) table.Row {
	return table.Row{
		w.Name,
		humanize.Ago(w.LastSuccess),
		humanize.Ago(w.LastFailure),
		strconv.Itoa(w.ConsecutiveFailures),
		w.LastError,
	}
}

// workersHeaderView renders the title bar with the worker count and how many
// are currently failing.
func (m *model) workersHeaderView() string {
	title := fmt.Sprintf("workers   %d registered", len(m.workers.list.received))
	if failing := failingWorkers(m.workers.list.received); failing > 0 {
		title += fmt.Sprintf("   %d failing", failing)
	}
	return m.titleBar(title)
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
	return m.paneBody(&m.workers.loadState, func() []pane {
		if len(m.workers.list.received) == 0 {
			return []pane{textPane(pathStyle.Render("(no workers registered)"))}
		}
		return []pane{m.workers.list.pane(m)}
	})
}

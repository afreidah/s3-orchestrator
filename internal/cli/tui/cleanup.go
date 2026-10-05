// -------------------------------------------------------------------------------
// TUI - Cleanup Queue View
//
// Author: Alex Freidah
//
// Pane over the cleanup queue and its dead-letter table: objects whose backend
// delete has not yet succeeded, and those that exhausted their retry budget and
// now need an operator. Both listings share one table, toggled with "t", so
// neither loses half the pane's height. Requeue is the pane's only write
// action, scoped to the selected row's backend. Reached with "u".
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"strconv"

	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// cleanupTab selects which of the pane's two listings is on screen.
type cleanupTab int

const (
	cleanupTabQueue cleanupTab = iota
	cleanupTabDLQ
)

// cleanupView holds the state of the cleanup pane. Both listings load together
// so the header can show both depths whichever tab is active.
type cleanupView struct {
	tab        cleanupTab                  // listing currently on screen
	queueDepth int64                       // total pending rows, which may exceed the loaded page
	dlqDepth   int64                       // total dead-lettered rows
	queueRows  []adminapi.CleanupQueueItem // loaded page of pending cleanups
	dlqRows    []adminapi.CleanupDLQItem   // loaded page of dead-lettered cleanups
	queue      table.Model                 // table over queueRows
	dlq        table.Model                 // table over dlqRows
	loaded     bool                        // a snapshot has arrived, so the depths are real
	loading    bool                        // a fetch is in flight
	err        error                       // last fetch error, if any
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// cleanupLoadedMsg carries both successfully loaded cleanup listings.
type cleanupLoadedMsg struct {
	queue *adminapi.CleanupQueueResponse
	dlq   *adminapi.CleanupDLQResponse
}

// cleanupErrMsg carries a failed cleanup fetch.
type cleanupErrMsg struct{ err error }

// cleanupRequeuedMsg carries the outcome of a dead-letter requeue.
type cleanupRequeuedMsg struct {
	resp *adminapi.CleanupDLQRequeueResponse
	err  error
}

// loadCleanup returns a command that fetches both listings off the main loop.
// They are fetched sequentially rather than concurrently because the pane
// cannot render a half-loaded state anyway, and either failure fails the load.
func (m *model) loadCleanup() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx := context.Background()
		queue, err := client.GetCleanupQueue(ctx)
		if err != nil {
			return cleanupErrMsg{err}
		}
		dlq, err := client.GetCleanupDLQ(ctx)
		if err != nil {
			return cleanupErrMsg{err}
		}
		return cleanupLoadedMsg{queue: queue, dlq: dlq}
	}
}

// requeueDLQ returns a command that moves one backend's dead-lettered rows back
// into the cleanup queue.
func (m *model) requeueDLQ(backend string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.RequeueCleanupDLQ(context.Background(), backend)
		return cleanupRequeuedMsg{resp: resp, err: err}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyCleanup folds both loaded listings into the pane state, keeping each
// table's highlighted row selected across the refresh.
func (m *model) applyCleanup(msg cleanupLoadedMsg) {
	prevQueue := keyAt(cleanupQueueKeys(m.cleanup.queueRows), m.cleanup.queue.Cursor())
	prevDLQ := keyAt(cleanupDLQKeys(m.cleanup.dlqRows), m.cleanup.dlq.Cursor())
	m.cleanup.queueDepth = msg.queue.Depth
	m.cleanup.dlqDepth = msg.dlq.Depth
	m.cleanup.queueRows = msg.queue.Items
	m.cleanup.dlqRows = msg.dlq.Items
	m.cleanup.queue.SetRows(rowsFromCleanupQueue(msg.queue.Items))
	m.cleanup.dlq.SetRows(rowsFromCleanupDLQ(msg.dlq.Items))
	reselect(&m.cleanup.queue, cleanupQueueKeys(msg.queue.Items), prevQueue)
	reselect(&m.cleanup.dlq, cleanupDLQKeys(msg.dlq.Items), prevDLQ)
	m.cleanup.loaded = true
	m.cleanup.loading = false
	m.cleanup.err = nil
}

// cleanupQueueKeys identifies queue rows by their row ID.
func cleanupQueueKeys(items []adminapi.CleanupQueueItem) []string {
	keys := make([]string, len(items))
	for i := range items {
		keys[i] = strconv.FormatInt(items[i].ID, 10)
	}
	return keys
}

// cleanupDLQKeys identifies dead-lettered rows by backend and object key,
// since the listing carries no row ID.
func cleanupDLQKeys(items []adminapi.CleanupDLQItem) []string {
	keys := make([]string, len(items))
	for i := range items {
		keys[i] = items[i].Backend + "\x00" + items[i].ObjectKey
	}
	return keys
}

// keyAt returns the key at index i, or "" when i is out of range.
func keyAt(keys []string, i int) string {
	if i < 0 || i >= len(keys) {
		return ""
	}
	return keys[i]
}

// applyCleanupRequeued reports the requeue outcome in the footer and reloads,
// so the row counts reflect the move.
func (m *model) applyCleanupRequeued(msg cleanupRequeuedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = &actionStatus{text: "requeue failed: " + msg.err.Error()}
		return m, nil
	}
	scope := "all backends"
	if msg.resp.Backend != "" {
		scope = msg.resp.Backend
	}
	m.status = &actionStatus{
		ok:   true,
		text: fmt.Sprintf("requeued %s from %s", countOf(int(msg.resp.Requeued), "row", "rows"), scope),
	}
	cmd := m.fetch(pollCleanup)
	return m, cmd
}

// handleCleanupKey applies pane keys (back, reload, tab switch, requeue) and
// delegates cursor movement to the active table.
func (m *model) handleCleanupKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "left", "h":
		return m.navBack()
	case "r":
		m.cleanup.loading = !m.cleanup.loaded
		cmd := m.fetch(pollCleanup)
		return m, cmd
	case "t":
		if m.cleanupOnDLQ() {
			m.cleanup.tab = cleanupTabQueue
		} else {
			m.cleanup.tab = cleanupTabDLQ
		}
		return m, nil
	case "R":
		return m.armRequeue()
	}

	var cmd tea.Cmd
	if m.cleanupOnDLQ() {
		m.cleanup.dlq, cmd = m.cleanup.dlq.Update(key)
	} else {
		m.cleanup.queue, cmd = m.cleanup.queue.Update(key)
	}
	return m, cmd
}

// cleanupOnDLQ reports whether the dead-letter listing is the active tab.
func (m *model) cleanupOnDLQ() bool { return m.cleanup.tab == cleanupTabDLQ }

// armRequeue confirms a requeue of every dead-lettered row for the selected
// row's backend. Requeue is a whole-backend operation, so the confirmation
// names the backend rather than the highlighted key.
func (m *model) armRequeue() (tea.Model, tea.Cmd) {
	if !m.cleanupOnDLQ() {
		m.status = &actionStatus{text: "requeue applies to the dead-letter listing (t to switch)"}
		return m, nil
	}
	idx := m.cleanup.dlq.Cursor()
	if idx < 0 || idx >= len(m.cleanup.dlqRows) {
		return m, nil
	}
	backend := m.cleanup.dlqRows[idx].Backend
	return m.startAction(adminAction{
		confirm: "Requeue every dead-lettered cleanup for backend " + backend + "?",
		run:     m.requeueDLQ(backend),
	})
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// cleanupQueueColumns and cleanupDLQColumns declare the two listings' columns.
// The key and the backend are what an operator acts on, so a narrow terminal
// drops the claim or move time first.
var (
	cleanupQueueColumns = []columnSpec{
		{title: "OBJECT KEY", min: 8, max: 60, priority: 5},
		{title: "BACKEND", min: 14, max: 14, priority: 4},
		{title: "SIZE", min: 10, max: 10, priority: 2},
		{title: "TRIES", min: 7, max: 7, priority: 3},
		{title: "CLAIMED", min: 10, max: 10, priority: 1},
	}
	cleanupDLQColumns = []columnSpec{
		{title: "OBJECT KEY", min: 8, max: 60, priority: 5},
		{title: "BACKEND", min: 14, max: 14, priority: 4},
		{title: "SIZE", min: 10, max: 10, priority: 2},
		{title: "TRIES", min: 7, max: 7, priority: 3},
		{title: "MOVED", min: 10, max: 10, priority: 1},
	}
)

// rowsFromCleanupQueue builds table rows from the pending listing, in the same
// order so the table cursor indexes straight into the rows.
func rowsFromCleanupQueue(items []adminapi.CleanupQueueItem) []table.Row {
	rows := make([]table.Row, 0, len(items))
	for i := range items {
		claimed := "-"
		if items[i].ClaimedBy != "" {
			claimed = items[i].ClaimedBy
		}
		rows = append(rows, table.Row{
			items[i].ObjectKey,
			items[i].Backend,
			humanize.Bytes(items[i].SizeBytes),
			strconv.FormatInt(int64(items[i].Attempts), 10),
			claimed,
		})
	}
	return rows
}

// rowsFromCleanupDLQ builds table rows from the dead-letter listing, in the
// same order so the table cursor indexes straight into the rows.
func rowsFromCleanupDLQ(items []adminapi.CleanupDLQItem) []table.Row {
	rows := make([]table.Row, 0, len(items))
	for i := range items {
		rows = append(rows, table.Row{
			items[i].ObjectKey,
			items[i].Backend,
			humanize.Bytes(items[i].SizeBytes),
			strconv.FormatInt(int64(items[i].Attempts), 10),
			tickAge(items[i].MovedAt),
		})
	}
	return rows
}

// cleanupPaneView composes the pane's full-screen layout.
func (m *model) cleanupPaneView() string {
	return m.frame(m.cleanupHeaderView(), m.cleanupFooterView(), m.cleanupBody()...)
}

// cleanupHeaderView renders the title bar with both depths, marking the active
// tab, so the size of the listing the user is not looking at stays visible.
func (m *model) cleanupHeaderView() string {
	queue := fmt.Sprintf("pending %d", m.cleanup.queueDepth)
	dlq := fmt.Sprintf("dead-letter %d", m.cleanup.dlqDepth)
	if m.cleanupOnDLQ() {
		dlq = "[" + dlq + "]"
	} else {
		queue = "[" + queue + "]"
	}
	return m.contentTitleStyle().Width(m.contentWidth()).Render("cleanup   " + queue + "   " + dlq)
}

// cleanupFooterView renders the cleanup key hints. Requeue is only offered on
// the listing it applies to.
func (m *model) cleanupFooterView() string {
	hints := "up/down move - t dead-letter - r reload - tab nav - q quit"
	if m.cleanupOnDLQ() {
		hints = "up/down move - t pending - R requeue backend - r reload - tab nav - q quit"
	}
	return m.footer(hints)
}

// cleanupBody renders the current content: an error, the loading indicator,
// an empty notice, or the active listing.
func (m *model) cleanupBody() []pane {
	return m.paneBody(m.cleanup.err, "", m.cleanup.loading, func() []pane {
		if m.cleanupOnDLQ() {
			if len(m.cleanup.dlqRows) == 0 {
				return []pane{textPane(statusOKStyle.Render("(no dead-lettered cleanups)"))}
			}
			return []pane{m.tablePane(&m.cleanup.dlq, cleanupDLQColumns)}
		}
		if len(m.cleanup.queueRows) == 0 {
			return []pane{textPane(statusOKStyle.Render("(cleanup queue is empty)"))}
		}
		return []pane{m.tablePane(&m.cleanup.queue, cleanupQueueColumns)}
	})
}

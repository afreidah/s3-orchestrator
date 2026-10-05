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
	tab        cleanupTab                           // listing currently on screen
	queueDepth int64                                // total pending rows, which may exceed the loaded page
	dlqDepth   int64                                // total dead-lettered rows
	queue      sortTable[adminapi.CleanupQueueItem] // loaded page of pending cleanups
	dlq        sortTable[adminapi.CleanupDLQItem]   // loaded page of dead-lettered cleanups
	loaded     bool                                 // a snapshot has arrived, so the depths are real
	loading    bool                                 // a fetch is in flight
	err        error                                // last fetch error, if any
}

// newCleanupView builds the pane's empty state. Queue rows are keyed by their
// row ID; dead-lettered rows carry none, so they are keyed by backend and
// object key.
func newCleanupView() cleanupView {
	return cleanupView{
		queue: newSortTable(cleanupQueueColumns, cleanupQueueSorts, rowsFromCleanupQueue,
			func(c *adminapi.CleanupQueueItem) string { return strconv.FormatInt(c.ID, 10) }),
		dlq: newSortTable(cleanupDLQColumns, cleanupDLQSorts, rowsFromCleanupDLQ,
			func(c *adminapi.CleanupDLQItem) string { return c.Backend + "\x00" + c.ObjectKey }),
	}
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
	m.cleanup.queueDepth = msg.queue.Depth
	m.cleanup.dlqDepth = msg.dlq.Depth
	m.cleanup.queue.setItems(msg.queue.Items)
	m.cleanup.dlq.setItems(msg.dlq.Items)
	m.cleanup.loaded = true
	m.cleanup.loading = false
	m.cleanup.err = nil
}

// applyCleanupRequeued reports the requeue outcome in the footer and reloads,
// so the row counts reflect the move.
func (m *model) applyCleanupRequeued(msg cleanupRequeuedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.report(false, "requeue failed: "+msg.err.Error())
		return m, nil
	}
	scope := "all backends"
	if msg.resp.Backend != "" {
		scope = msg.resp.Backend
	}
	m.report(true, fmt.Sprintf("requeued %s from %s", countOf(int(msg.resp.Requeued), "row", "rows"), scope))
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

	if m.cleanupOnDLQ() {
		cmd := m.cleanup.dlq.update(key)
		return m, cmd
	}
	cmd := m.cleanup.queue.update(key)
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
	item, ok := m.cleanup.dlq.selected()
	if !ok {
		return m, nil
	}
	backend := item.Backend
	return m.startAction(adminAction{
		confirm: "Requeue every dead-lettered cleanup for backend " + backend + "?",
		run:     m.requeueDLQ(backend),
	})
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// Titles of the columns both cleanup listings share, used by the column specs
// and the comparators alike.
const (
	colObjectKey = "OBJECT KEY"
	colBackend   = "BACKEND"
	colSize      = "SIZE"
	colTries     = "TRIES"
)

// cleanupQueueColumns and cleanupDLQColumns declare the two listings' columns.
// The key and the backend are what an operator acts on, so a narrow terminal
// drops the claim or move time first.
var (
	cleanupQueueColumns = []columnSpec{
		{title: colObjectKey, min: 8, max: 60, priority: 5},
		{title: colBackend, min: 14, max: 14, priority: 4},
		{title: colSize, min: 10, max: 10, priority: 2},
		{title: colTries, min: 7, max: 7, priority: 3},
		{title: "CLAIMED", min: 10, max: 10, priority: 1},
	}
	cleanupDLQColumns = []columnSpec{
		{title: colObjectKey, min: 8, max: 60, priority: 5},
		{title: colBackend, min: 14, max: 14, priority: 4},
		{title: colSize, min: 10, max: 10, priority: 2},
		{title: colTries, min: 7, max: 7, priority: 3},
		{title: "MOVED", min: 10, max: 10, priority: 1},
	}
)

// cleanupQueueSorts and cleanupDLQSorts order the two listings by every
// column. MOVED sorts by when the row was dead-lettered, so an ascending sort
// puts the oldest first.
var (
	cleanupQueueSorts = map[string]func(a, b *adminapi.CleanupQueueItem) int{
		colObjectKey: by(func(c *adminapi.CleanupQueueItem) string { return c.ObjectKey }),
		colBackend:   by(func(c *adminapi.CleanupQueueItem) string { return c.Backend }),
		colSize:      by(func(c *adminapi.CleanupQueueItem) int64 { return c.SizeBytes }),
		colTries:     by(func(c *adminapi.CleanupQueueItem) int32 { return c.Attempts }),
		"CLAIMED":    by(func(c *adminapi.CleanupQueueItem) string { return c.ClaimedBy }),
	}
	cleanupDLQSorts = map[string]func(a, b *adminapi.CleanupDLQItem) int{
		colObjectKey: by(func(c *adminapi.CleanupDLQItem) string { return c.ObjectKey }),
		colBackend:   by(func(c *adminapi.CleanupDLQItem) string { return c.Backend }),
		colSize:      by(func(c *adminapi.CleanupDLQItem) int64 { return c.SizeBytes }),
		colTries:     by(func(c *adminapi.CleanupDLQItem) int32 { return c.Attempts }),
		"MOVED":      by(func(c *adminapi.CleanupDLQItem) int64 { return c.MovedAt.UnixNano() }),
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
	return m.frame(m.cleanupHeaderView(), m.hintFooter(), m.cleanupBody()...)
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

// cleanupBody renders the current content: an error, the loading indicator,
// an empty notice, or the active listing.
func (m *model) cleanupBody() []pane {
	return m.paneBody(m.cleanup.err, "", m.cleanup.loading, func() []pane {
		if m.cleanupOnDLQ() {
			if len(m.cleanup.dlq.received) == 0 {
				return []pane{textPane(statusOKStyle.Render("(no dead-lettered cleanups)"))}
			}
			return []pane{m.cleanup.dlq.pane(m)}
		}
		if len(m.cleanup.queue.received) == 0 {
			return []pane{textPane(statusOKStyle.Render("(cleanup queue is empty)"))}
		}
		return []pane{m.cleanup.queue.pane(m)}
	})
}

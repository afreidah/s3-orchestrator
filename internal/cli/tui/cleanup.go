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
	"cmp"
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
	loadState
	tab        cleanupTab                           // listing currently on screen
	queueDepth int64                                // total pending rows, which may exceed the loaded page
	dlqDepth   int64                                // total dead-lettered rows
	queue      sortTable[adminapi.CleanupQueueItem] // loaded page of pending cleanups
	dlq        sortTable[adminapi.CleanupDLQItem]   // loaded page of dead-lettered cleanups
	fetched    bool                                 // a snapshot has arrived, so the depths are real
}

// newCleanupView builds the pane's empty state. Queue rows are keyed by their
// row ID; dead-lettered rows carry none, so they are keyed by backend and
// object key.
func newCleanupView() cleanupView {
	return cleanupView{
		queue: newSortTable(cleanupQueueColumns, cleanupQueueSorts, cleanupQueueRow,
			func(c *adminapi.CleanupQueueItem) string { return strconv.FormatInt(c.ID, 10) }),
		dlq: newSortTable(cleanupDLQColumns, cleanupDLQSorts, cleanupDLQRow,
			func(c *adminapi.CleanupDLQItem) string { return c.Backend + "\x00" + c.ObjectKey }),
	}
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// cleanupListings is both loaded cleanup listings.
type cleanupListings struct {
	queue *adminapi.CleanupQueueResponse
	dlq   *adminapi.CleanupDLQResponse
}

// loadCleanup returns a command that fetches both listings off the main loop.
// They are fetched sequentially rather than concurrently because the pane
// cannot render a half-loaded state anyway, and either failure fails the load.
func (m *model) loadCleanup() tea.Cmd {
	client := m.client
	return fetchCmd(pollCleanup, func(ctx context.Context) (cleanupListings, error) {
		queue, err := client.GetCleanupQueue(ctx)
		if err != nil {
			return cleanupListings{}, err
		}
		dlq, err := client.GetCleanupDLQ(ctx)
		return cleanupListings{queue: queue, dlq: dlq}, err
	}, (*model).applyCleanup, func(m *model, err error) { m.cleanup.failed(err) })
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyCleanup folds both loaded listings into the pane state, keeping each
// table's highlighted row selected across the refresh.
func (m *model) applyCleanup(msg cleanupListings) {
	m.cleanup.queueDepth = msg.queue.Depth
	m.cleanup.dlqDepth = msg.dlq.Depth
	m.cleanup.queue.setItems(msg.queue.Items)
	m.cleanup.dlq.setItems(msg.dlq.Items)
	m.cleanup.fetched = true
	m.cleanup.loaded()
}

// handleCleanupKey applies pane keys (tab switch, requeue) and
// delegates cursor movement to the active table.
func (m *model) handleCleanupKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
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

// armRequeue runs the backend menu's requeue against the selected row's
// backend. Requeue is a whole-backend operation, so the confirmation names the
// backend rather than the highlighted key.
func (m *model) armRequeue() (tea.Model, tea.Cmd) {
	if !m.cleanupOnDLQ() {
		m.status = &actionStatus{text: "requeue applies to the dead-letter listing (t to switch)"}
		return m, nil
	}
	item, ok := m.cleanup.dlq.selected()
	if !ok {
		return m, nil
	}
	return m.runBackendAction(adminapi.PathCleanupDLQRequeue, item.Backend)
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

// cleanupQueueRow renders one pending cleanup, with a dash for a row no
// instance has claimed.
func cleanupQueueRow(c *adminapi.CleanupQueueItem) table.Row {
	return table.Row{
		c.ObjectKey,
		c.Backend,
		humanize.Bytes(c.SizeBytes),
		strconv.FormatInt(int64(c.Attempts), 10),
		cmp.Or(c.ClaimedBy, "-"),
	}
}

// cleanupDLQRow renders one dead-lettered cleanup.
func cleanupDLQRow(c *adminapi.CleanupDLQItem) table.Row {
	return table.Row{
		c.ObjectKey,
		c.Backend,
		humanize.Bytes(c.SizeBytes),
		strconv.FormatInt(int64(c.Attempts), 10),
		humanize.Ago(c.MovedAt),
	}
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
	return m.titleBar("cleanup   " + queue + "   " + dlq)
}

// cleanupBody renders the current content: an error, the loading indicator,
// an empty notice, or the active listing.
func (m *model) cleanupBody() []pane {
	return m.paneBody(&m.cleanup.loadState, func() []pane {
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

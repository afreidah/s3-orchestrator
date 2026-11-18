// -------------------------------------------------------------------------------
// TUI - Integrity View
//
// Author: Alex Freidah
//
// Pane over the copies the integrity passes act on: those with no content hash,
// which backfill hashes when the orchestrator manages them, and those encrypted
// with no key, which only a purge clears. Both listings share one table,
// toggled with "t", and "m" narrows the unhashed one to the managed or the
// unmanaged copies, since a page in key order can otherwise be all of one
// kind. Reached with "i".
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// integrityTab selects which of the pane's two listings is on screen.
type integrityTab int

const (
	integrityTabUnhashed integrityTab = iota
	integrityTabUnreadable
)

// integrityView holds the state of the integrity pane. Both listings load
// together so the header can show both totals whichever tab is active.
// managed narrows the unhashed listing, nil listing both kinds.
type integrityView struct {
	loadState
	tab             integrityTab
	managed         *bool
	unhashedTotal   int64
	unreadableTotal int64
	unhashed        sortTable[adminapi.ListedCopy]
	unreadable      sortTable[adminapi.ListedCopy]
	fetched         bool
}

// newIntegrityView builds the pane's empty state. A copy is one key on one
// backend, so the two together key a row.
func newIntegrityView() integrityView {
	listing := func() sortTable[adminapi.ListedCopy] {
		return newSortTable(copyColumns, copySorts, copyRow,
			func(c *adminapi.ListedCopy) string { return c.Backend + "\x00" + c.Key })
	}
	return integrityView{unhashed: listing(), unreadable: listing()}
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// integrityListings is both loaded listings, and the management filter the
// unhashed one was asked for.
type integrityListings struct {
	managed    *bool
	unhashed   *adminapi.CopyListResponse
	unreadable *adminapi.CopyListResponse
}

// loadIntegrity returns a command that fetches both listings off the main loop,
// one after the other, since the pane cannot render a half-loaded state.
func (m *model) loadIntegrity() tea.Cmd {
	client, managed := m.client, m.integrity.managed
	return fetchCmd(pollIntegrity, func(ctx context.Context) (integrityListings, error) {
		unhashed, err := client.ListUnhashed(ctx, managed)
		if err != nil {
			return integrityListings{}, err
		}
		unreadable, err := client.ListUnreadable(ctx)
		return integrityListings{managed: managed, unhashed: unhashed, unreadable: unreadable}, err
	}, (*model).applyIntegrity, func(m *model, err error) { m.integrity.failed(err) })
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyIntegrity folds both loaded listings into the pane state, keeping each
// table's highlighted row selected across the refresh. A load asked for under
// a filter the operator has since changed is dropped, since the load the
// change started is the one that answers it.
func (m *model) applyIntegrity(msg integrityListings) {
	if !sameManaged(msg.managed, m.integrity.managed) {
		return
	}
	m.integrity.unhashedTotal = msg.unhashed.Total
	m.integrity.unreadableTotal = msg.unreadable.Total
	m.integrity.unhashed.setItems(msg.unhashed.Copies)
	m.integrity.unreadable.setItems(msg.unreadable.Copies)
	m.integrity.fetched = true
	m.integrity.loaded()
}

// handleIntegrityKey applies pane keys (tab switch, management filter) and
// delegates cursor movement to the active table.
func (m *model) handleIntegrityKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "t":
		if m.integrityOnUnreadable() {
			m.integrity.tab = integrityTabUnhashed
		} else {
			m.integrity.tab = integrityTabUnreadable
		}
		return m, nil
	case "m":
		if m.integrityOnUnreadable() {
			m.status = &actionStatus{text: "the managed filter applies to the unhashed listing (t to switch)"}
			return m, nil
		}
		m.integrity.managed = nextManaged(m.integrity.managed)
		m.integrity.loading = true
		cmd := m.fetchAt(pollIntegrity, time.Now())
		return m, cmd
	}

	if m.integrityOnUnreadable() {
		cmd := m.integrity.unreadable.update(key)
		return m, cmd
	}
	cmd := m.integrity.unhashed.update(key)
	return m, cmd
}

// integrityOnUnreadable reports whether the unreadable listing is the active
// tab.
func (m *model) integrityOnUnreadable() bool { return m.integrity.tab == integrityTabUnreadable }

// nextManaged steps the management filter from both kinds to the managed
// copies, to the unmanaged ones, and back to both.
func nextManaged(managed *bool) *bool {
	switch {
	case managed == nil:
		return new(true)
	case *managed:
		return new(false)
	default:
		return nil
	}
}

// sameManaged reports whether two management filters select the same copies.
func sameManaged(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// Titles of the columns only the copy listings carry.
const (
	colManaged   = "MANAGED"
	colEncrypted = "ENCRYPTED"
	colCreated   = "CREATED"
)

// copyColumns declares both listings' columns. The key and the backend name the
// copy, so a narrow terminal drops when it was written first.
var copyColumns = []columnSpec{
	{title: colObjectKey, min: 8, max: 60, priority: 6},
	{title: colBackend, min: 14, max: 14, priority: 5},
	{title: colSize, min: 10, max: 10, priority: 4},
	{title: colManaged, min: 9, max: 9, priority: 3},
	{title: colEncrypted, min: 10, max: 10, priority: 2},
	{title: colCreated, min: 10, max: 10, priority: 1},
}

// copySorts orders both listings by the columns that hold a value rather than
// a flag. CREATED sorts by when the copy was written, so an ascending sort
// puts the oldest first.
var copySorts = map[string]func(a, b *adminapi.ListedCopy) int{
	colObjectKey: by(func(c *adminapi.ListedCopy) string { return c.Key }),
	colBackend:   by(func(c *adminapi.ListedCopy) string { return c.Backend }),
	colSize:      by(func(c *adminapi.ListedCopy) int64 { return c.SizeBytes }),
	colCreated:   by(func(c *adminapi.ListedCopy) int64 { return c.CreatedAt.UnixNano() }),
}

// copyRow renders one listed copy.
func copyRow(c *adminapi.ListedCopy) table.Row {
	return table.Row{
		c.Key,
		c.Backend,
		humanize.Bytes(c.SizeBytes),
		strconv.FormatBool(c.Managed),
		strconv.FormatBool(c.Encrypted),
		humanize.Ago(c.CreatedAt),
	}
}

// managedLabel names the copies a management filter selects.
func managedLabel(managed *bool) string {
	switch {
	case managed == nil:
		return "all"
	case *managed:
		return "managed"
	default:
		return "unmanaged"
	}
}

// integrityHeaderView renders the title bar with both totals, marking the
// active tab, and the filter the unhashed total is counted under.
func (m *model) integrityHeaderView() string {
	unhashed := fmt.Sprintf("unhashed %s (%s)", humanize.Comma(m.integrity.unhashedTotal), managedLabel(m.integrity.managed))
	unreadable := "unreadable " + humanize.Comma(m.integrity.unreadableTotal)
	if m.integrityOnUnreadable() {
		unreadable = "[" + unreadable + "]"
	} else {
		unhashed = "[" + unhashed + "]"
	}
	return m.titleBar("integrity   " + unhashed + "   " + unreadable)
}

// integrityBody renders the current content: an error, the loading indicator,
// an empty notice, or the active listing.
func (m *model) integrityBody() []pane {
	return m.paneBody(&m.integrity.loadState, func() []pane {
		if m.integrityOnUnreadable() {
			if len(m.integrity.unreadable.received) == 0 {
				return []pane{textPane(statusOKStyle.Render("(no unreadable copies)"))}
			}
			return []pane{m.integrity.unreadable.pane(m)}
		}
		if len(m.integrity.unhashed.received) == 0 {
			return []pane{textPane(statusOKStyle.Render("(no unhashed copies)"))}
		}
		return []pane{m.integrity.unhashed.pane(m)}
	})
}

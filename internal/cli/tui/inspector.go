// -------------------------------------------------------------------------------
// TUI - Object Inspector
//
// Author: Alex Freidah
//
// Detail pane for a single object key. Fetches every backend copy from the
// admin object-locations endpoint and renders the per-copy ledger (backend,
// size, age, encryption, content hash) so an operator can see exactly where an
// object lives and how its replicas compare. Read-only, like the browser.
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

// viewMode selects which pane the model is showing.
type viewMode int

const (
	// modeBrowse shows the prefix listing; modeInspect shows one object's copies.
	modeBrowse viewMode = iota
	modeInspect
)

// inspector holds the state of the object-detail pane.
type inspector struct {
	key       string                    // the object key under inspection
	locations []adminapi.ObjectLocation // per-backend copies, in load order
	table     table.Model               // scrolling table over the copies
	loading   bool                      // a locations fetch is in flight
	err       error                     // last fetch error, if any
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// locationsLoadedMsg carries a successfully loaded location ledger.
type locationsLoadedMsg struct {
	resp *adminapi.ObjectLocationsResponse
}

// locationsErrMsg carries a failed locations fetch.
type locationsErrMsg struct{ err error }

// loadLocations returns a command that fetches every copy of key off the main
// loop, delivering the result back as a locationsLoadedMsg or locationsErrMsg.
func (m *model) loadLocations(key string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.GetObjectLocations(context.Background(), key)
		if err != nil {
			return locationsErrMsg{err}
		}
		return locationsLoadedMsg{resp}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// openInspector switches to the inspector pane for key and kicks off its load.
func (m *model) openInspector(key string) (tea.Model, tea.Cmd) {
	m.mode = modeInspect
	m.insp = inspector{key: key, loading: true, table: newTable()}
	m.resizeInspector()
	cmd := m.loadLocations(key)
	return m, cmd
}

// applyLocations folds a loaded ledger into the inspector state.
func (m *model) applyLocations(resp *adminapi.ObjectLocationsResponse) {
	m.insp.locations = resp.Locations
	m.insp.table.SetRows(rowsFromLocations(resp.Locations))
	m.insp.table.SetCursor(0)
	m.insp.loading = false
	m.insp.err = nil
}

// handleInspectKey applies inspector-level keys (quit, back, reload) and
// delegates cursor movement to the table.
func (m *model) handleInspectKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "esc", "backspace", "left", "h":
		m.mode = modeBrowse
		return m, nil
	case "r":
		m.insp.loading = true
		cmd := m.loadLocations(m.insp.key)
		return m, cmd
	}

	var cmd tea.Cmd
	m.insp.table, cmd = m.insp.table.Update(key)
	return m, cmd
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// resizeInspector fits the inspector columns and viewport to the window, giving
// the backend column whatever width the fixed columns leave behind.
func (m *model) resizeInspector() {
	const fixed = 12 + 14 + 5 + 12 + 18 // size + created + enc + key id + hash
	backendWidth := max(m.width-fixed-2, 8)
	m.insp.table.SetColumns([]table.Column{
		{Title: "BACKEND", Width: backendWidth},
		{Title: "SIZE", Width: 12},
		{Title: "CREATED", Width: 14},
		{Title: "ENC", Width: 5},
		{Title: "KEY ID", Width: 12},
		{Title: "HASH", Width: 18},
	})
	m.insp.table.SetWidth(m.width)
	m.insp.table.SetHeight(max(m.height-2, 3))
}

// rowsFromLocations builds inspector rows in the same order as the ledger so
// the table cursor indexes straight into the copies.
func rowsFromLocations(locations []adminapi.ObjectLocation) []table.Row {
	rows := make([]table.Row, 0, len(locations))
	for i := range locations {
		l := locations[i]
		rows = append(rows, table.Row{
			l.Backend,
			humanSize(l.SizeBytes),
			relativeAge(l.CreatedAt),
			yesNo(l.Encrypted),
			truncate(l.KeyID, 10),
			truncate(l.ContentHash, 16),
		})
	}
	return rows
}

// inspectView composes the inspector's full-screen layout.
func (m *model) inspectView() string {
	return m.frame(m.inspectHeaderView(), m.inspectFooterView(), m.inspectBodyView())
}

// inspectHeaderView renders the title bar with the key and copy count.
func (m *model) inspectHeaderView() string {
	title := fmt.Sprintf("inspect   %s   (%d copies)", m.insp.key, len(m.insp.locations))
	return titleStyle.Width(m.width).Render(title)
}

// inspectFooterView renders the inspector key hints.
func (m *model) inspectFooterView() string {
	return helpStyle.Width(m.width).Render("up/down move - esc back - r reload - q quit")
}

// inspectBodyView renders the current content: an error, the loading indicator,
// an empty notice, or the copy table.
func (m *model) inspectBodyView() string {
	switch {
	case m.insp.err != nil:
		return errStyle.Render("error: " + m.insp.err.Error())
	case m.insp.loading:
		return m.spinner.View() + " loading..."
	case len(m.insp.locations) == 0:
		return pathStyle.Render("(no copies found)")
	default:
		return m.insp.table.View()
	}
}

// -------------------------------------------------------------------------
// FORMATTING HELPERS
// -------------------------------------------------------------------------

// relativeAge renders how long ago t was in a compact, coarse form. A zero time
// renders as a dash.
func relativeAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}

// yesNo renders a boolean as a compact yes/no.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// truncate shortens s to n runes, marking the cut with a trailing tilde.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "~"
}

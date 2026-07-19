// -------------------------------------------------------------------------------
// TUI - Backends View
//
// Author: Alex Freidah
//
// Read-only status pane over the configured backends. Fetches the admin status
// snapshot and renders one row per backend: quota usage, object count,
// circuit-breaker health, drain state, and per-period API/egress/ingress
// counters. Reached with "b" from the browser; "esc" returns to the listing.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"strconv"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// backendsView holds the state of the backends status pane.
type backendsView struct {
	rows        []adminapi.BackendStatus // one entry per configured backend
	table       table.Model              // scrolling table over the backends
	dbHealthy   bool                     // metadata database health
	usagePeriod string                   // period the usage counters cover
	loading     bool                     // a status fetch is in flight
	err         error                    // last fetch error, if any
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// statusLoadedMsg carries a successfully loaded status snapshot.
type statusLoadedMsg struct{ resp *adminapi.StatusResponse }

// statusErrMsg carries a failed status fetch.
type statusErrMsg struct{ err error }

// loadStatus returns a command that fetches the status snapshot off the main
// loop, delivering the result back as a statusLoadedMsg or statusErrMsg.
func (m *model) loadStatus() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.GetStatus(context.Background())
		if err != nil {
			return statusErrMsg{err}
		}
		return statusLoadedMsg{resp}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyStatus folds a loaded snapshot into the backends state.
func (m *model) applyStatus(resp *adminapi.StatusResponse) {
	m.backends.rows = resp.Backends
	m.backends.dbHealthy = resp.DBHealthy
	m.backends.usagePeriod = resp.UsagePeriod
	m.backends.table.SetRows(rowsFromBackends(resp.Backends))
	m.backends.table.SetCursor(0)
	m.backends.loading = false
	m.backends.err = nil
}

// handleBackendsKey applies backends-level keys (quit, back, reload) and
// delegates cursor movement to the table.
func (m *model) handleBackendsKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "left", "h":
		m.navFocus = true
		m.navCursor = int(m.section)
		return m, nil
	case "r":
		m.backends.loading = true
		cmd := m.loadStatus()
		return m, cmd
	}

	var cmd tea.Cmd
	m.backends.table, cmd = m.backends.table.Update(key)
	return m, cmd
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// resizeBackends fits the backends columns and viewport to the window, capping
// the name column so short backend names don't sprawl on a wide terminal.
func (m *model) resizeBackends() {
	const (
		fixed   = 9 + 8 + 12 + 12 + 10 + 10 + 12 + 12 // health..egress
		cols    = 9
		nameCap = 24
	)
	nameWidth := fitFirstColumn(m.contentWidth(), fixed, cols, nameCap)
	m.backends.table.SetColumns([]table.Column{
		{Title: "BACKEND", Width: nameWidth},
		{Title: "HEALTH", Width: 9},
		{Title: "DRAIN", Width: 8},
		{Title: "USED", Width: 12},
		{Title: "LIMIT", Width: 12},
		{Title: "OBJECTS", Width: 10},
		{Title: "API", Width: 10},
		{Title: "INGRESS", Width: 12},
		{Title: "EGRESS", Width: 12},
	})
	m.backends.table.SetWidth(m.contentWidth())
	m.backends.table.SetHeight(max(m.height-2, 3))
}

// rowsFromBackends builds table rows from the status snapshot, in the same
// order so the table cursor indexes straight into rows.
func rowsFromBackends(backends []adminapi.BackendStatus) []table.Row {
	rows := make([]table.Row, 0, len(backends))
	for i := range backends {
		b := backends[i]
		limit := "-"
		if b.BytesLimit > 0 {
			limit = humanSize(b.BytesLimit)
		}
		rows = append(rows, table.Row{
			b.Name,
			backendHealth(b.Healthy),
			backendDrain(b.Draining),
			humanSize(b.BytesUsed),
			limit,
			strconv.FormatInt(b.ObjectCount, 10),
			strconv.FormatInt(b.APIRequests, 10),
			humanSize(b.IngressBytes),
			humanSize(b.EgressBytes),
		})
	}
	return rows
}

// backendHealth renders a backend's circuit-breaker state.
func backendHealth(healthy bool) string {
	if healthy {
		return "healthy"
	}
	return "unhealthy"
}

// backendDrain renders whether a backend is draining.
func backendDrain(draining bool) string {
	if draining {
		return "draining"
	}
	return "-"
}

// backendsView composes the pane's full-screen layout.
func (m *model) backendsPaneView() string {
	return m.frame(m.backendsHeaderView(), m.backendsFooterView(), m.backendsBodyView())
}

// backendsHeaderView renders the title bar with the backend count and DB health.
func (m *model) backendsHeaderView() string {
	title := fmt.Sprintf("backends   %d configured   db: %s", len(m.backends.rows), backendHealth(m.backends.dbHealthy))
	if m.backends.usagePeriod != "" {
		title += "   usage period: " + m.backends.usagePeriod
	}
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title)
}

// backendsFooterView renders the backends key hints.
func (m *model) backendsFooterView() string {
	return m.footer("up/down move - tab nav - r reload - q quit")
}

// backendsBodyView renders the current content: an error, the loading
// indicator, an empty notice, or the backends table.
func (m *model) backendsBodyView() string {
	switch {
	case m.backends.err != nil:
		return errStyle.Render("error: " + m.backends.err.Error())
	case m.backends.loading:
		return m.spinner.View() + " loading..."
	case len(m.backends.rows) == 0:
		return pathStyle.Render("(no backends)")
	default:
		return m.backends.table.View()
	}
}

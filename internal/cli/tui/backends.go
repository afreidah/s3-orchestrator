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

	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

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
	healthy := resp.DBHealthy
	m.dbHealthy = &healthy // surface globally for the sidebar indicator
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
		fixed   = 9 + 8 + 12 + 12 + 6 + 10 + 10 + 12 + 12 // health..egress incl use%
		cols    = 10
		nameCap = 24
	)
	nameWidth := fitFirstColumn(m.contentWidth(), fixed, cols, nameCap)
	m.backends.table.SetColumns([]table.Column{
		{Title: "BACKEND", Width: nameWidth},
		{Title: "HEALTH", Width: 9},
		{Title: "DRAIN", Width: 8},
		{Title: "USED", Width: 12},
		{Title: "LIMIT", Width: 12},
		{Title: "USE%", Width: 6},
		{Title: "OBJECTS", Width: 10},
		{Title: "API", Width: 10},
		{Title: "INGRESS", Width: 12},
		{Title: "EGRESS", Width: 12},
	})
	m.backends.table.SetWidth(m.contentWidth())
	m.backends.table.SetHeight(max(m.height-3, 3)) // 2-line header + 1-line footer
}

// rowsFromBackends builds table rows from the status snapshot, in the same
// order so the table cursor indexes straight into rows.
func rowsFromBackends(backends []adminapi.BackendStatus) []table.Row {
	rows := make([]table.Row, 0, len(backends))
	for i := range backends {
		b := backends[i]
		limit, usePct := "-", "-"
		if b.BytesLimit > 0 {
			limit = humanize.Bytes(b.BytesLimit)
			usePct = fmt.Sprintf("%d%%", usagePercent(b.BytesUsed, b.BytesLimit))
		}
		rows = append(rows, table.Row{
			b.Name,
			backendHealth(b.Healthy),
			backendDrain(b.Draining),
			humanize.Bytes(b.BytesUsed),
			limit,
			usePct,
			strconv.FormatInt(b.ObjectCount, 10),
			strconv.FormatInt(b.APIRequests, 10),
			humanize.Bytes(b.IngressBytes),
			humanize.Bytes(b.EgressBytes),
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
	title := fmt.Sprintf("backends   %d configured", len(m.backends.rows))
	if m.backends.usagePeriod != "" {
		title += "   usage period: " + m.backends.usagePeriod
	}
	titleLine := m.contentTitleStyle().Width(m.contentWidth()).Render(title)
	return titleLine + "\n" + m.backendsStatsLine()
}

// backendsStatsLine renders the coloured DB-health + total-usage line beneath
// the backends title. Kept out of the title bar so the colours are not fighting
// its background.
func (m *model) backendsStatsLine() string {
	db := statusOKStyle.Render("healthy")
	if !m.backends.dbHealthy {
		db = statusErrStyle.Render("UNAVAILABLE")
	}

	var used, limit int64
	for i := range m.backends.rows {
		used += m.backends.rows[i].BytesUsed
		if m.backends.rows[i].BytesLimit > 0 {
			limit += m.backends.rows[i].BytesLimit
		}
	}
	total := "total: " + humanize.Bytes(used)
	if limit > 0 {
		pct := usagePercent(used, limit)
		total = fmt.Sprintf("total: %s / %s (%s)",
			humanize.Bytes(used), humanize.Bytes(limit), usageStyle(pct).Render(fmt.Sprintf("%d%%", pct)))
	}
	return fmt.Sprintf("db: %s   %s", db, total)
}

// usagePercent returns used as a whole-number percentage of limit (0 when
// limit is non-positive).
func usagePercent(used, limit int64) int {
	if limit <= 0 {
		return 0
	}
	return int(used * 100 / limit)
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

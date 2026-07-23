// -------------------------------------------------------------------------------
// TUI - Left Navigation
//
// Author: Alex Freidah
//
// The persistent left nav bar and the top-level section model. Sections are the
// nav destinations (Files, Backends, and a Logs placeholder); the active
// section drives what the content area to the right renders. The nav can take
// focus (tab) for arrow-key selection, and letter shortcuts jump directly.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// sidebarWidth is the fixed content width of the left nav (excluding its border).
const sidebarWidth = 14

// section is a top-level nav destination.
type section int

const (
	sectionFiles section = iota
	sectionBackends
	sectionReplication
	sectionLogs
	sectionOps
)

// navEntry is one row in the left nav.
type navEntry struct {
	label   string
	sec     section
	enabled bool
}

// navEntries lists the nav rows in display order. Disabled entries render as
// placeholders and are skipped by cursor navigation.
func navEntries() []navEntry {
	return []navEntry{
		{"Files", sectionFiles, true},
		{"Backends", sectionBackends, true},
		{"Replication", sectionReplication, true},
		{"Logs", sectionLogs, true},
		{"Ops", sectionOps, true},
	}
}

// selectableSections is the number of enabled nav destinations; it bounds the
// nav cursor.
const selectableSections = 5

// contentWidth is the width available to the content area beside the nav.
func (m *model) contentWidth() int {
	if w := m.width - sidebarWidth - 1; w > 24 {
		return w
	}
	return 24
}

// tableCellPad is the horizontal padding the bubbles table adds to every cell
// (one column on each side); the rendered table is this much wider per column
// than the sum of the declared column widths.
const tableCellPad = 2

// fitFirstColumn sizes a table's leading name column to fill whatever the fixed
// columns and per-cell padding leave, capped so short names don't sprawl across
// a wide terminal and floored so it never collapses. cols is the total column
// count (including the name column) so the padding budget is exact.
func fitFirstColumn(contentWidth, fixedSum, cols, maxWidth int) int {
	budget := contentWidth - fixedSum - cols*tableCellPad
	if budget > maxWidth {
		return maxWidth
	}
	if budget < 8 {
		return 8
	}
	return budget
}

// selectSection switches the active section, drops nav focus, and loads the
// section's data when entering it needs a fetch.
func (m *model) selectSection(s section) (tea.Model, tea.Cmd) {
	m.section = s
	m.navFocus = false
	m.navCursor = int(s)
	switch s {
	case sectionBackends:
		m.backends = backendsView{loading: true, table: newTable()}
		m.resizeBackends()
		cmd := m.loadStatus()
		return m, cmd
	case sectionReplication:
		return m.enterReplication()
	case sectionOps:
		m.ops.showOut = false
		m.resizeOps()
		return m, nil
	case sectionLogs:
		m.logs = logsView{loading: true}
		m.resizeLogs()
		cmd := m.loadLogs()
		return m, cmd
	}
	return m, nil
}

// handleNavKey drives the focused sidebar: move the highlight, open a section,
// or drop focus back to the content.
func (m *model) handleNavKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "up", "k":
		if m.navCursor > 0 {
			m.navCursor--
		}
	case "down", "j":
		if m.navCursor < selectableSections-1 {
			m.navCursor++
		}
	case "enter", "right", "l":
		return m.selectSection(section(m.navCursor))
	case "esc":
		m.navFocus = false
	}
	return m, nil
}

// sidebarView renders the left nav: the app tag then one row per section, with a
// ">" marker on the active entry (or, while the nav is focused, the highlight).
func (m *model) sidebarView() string {
	var b strings.Builder
	title := navTitleStyle
	if !m.navFocus {
		title = titleMutedStyle
	}
	b.WriteString(title.Render("s3o"))
	b.WriteString("\n\n")
	for i, e := range navEntries() {
		active := (!m.navFocus && e.enabled && e.sec == m.section) || (m.navFocus && i == m.navCursor)
		marker := "  "
		if active {
			marker = "> "
		}
		label := e.label
		if !e.enabled {
			label += " (soon)"
		}
		style := navItemStyle
		switch {
		case active:
			style = navActiveStyle
		case !e.enabled:
			style = navDisabledStyle
		}
		b.WriteString(style.Render(marker + label))
		b.WriteString("\n")
	}

	// Persistent DB-health indicator, below a divider, visible from any section.
	b.WriteString(navDisabledStyle.Render(strings.Repeat("-", sidebarWidth-2)))
	b.WriteString("\n")
	b.WriteString(m.dbIndicator())

	divider := lipgloss.Color("240")
	if m.navFocus {
		divider = lipgloss.Color("39")
	}
	return sidebarStyle.BorderForeground(divider).Width(sidebarWidth).Height(m.height).Render(b.String())
}

// dbIndicator renders the metadata DB health for the sidebar: green when
// healthy, red when down, faint when not yet known.
func (m *model) dbIndicator() string {
	switch {
	case m.dbHealthy == nil:
		return navDisabledStyle.Render("db ?")
	case *m.dbHealthy:
		return statusOKStyle.Render("db ok")
	default:
		return statusErrStyle.Render("db DOWN")
	}
}

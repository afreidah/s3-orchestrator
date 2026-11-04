// -------------------------------------------------------------------------------
// TUI - Left Navigation
//
// Author: Alex Freidah
//
// The persistent left nav bar and the top-level section model. Sections are the
// nav destinations (Dashboard, Files, Backends, and the status panes); the
// active section drives what the content area to the right renders. The TUI
// opens on the Dashboard. The nav can take focus (tab) for arrow-key
// selection, and letter shortcuts jump directly.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// sidebarWidth is the fixed content width of the left nav (excluding its border).
// It must fit the widest row: a two-column marker plus the longest label
// ("Replication"), else lipgloss soft-wraps the row and mangles the layout.
const sidebarWidth = 16

// contentWidth is the width available to the content area beside the nav.
func (m *model) contentWidth() int {
	if w := m.width - sidebarWidth - 1; w > 24 {
		return w
	}
	return 24
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// navBack hands focus back to the nav with the cursor on the section the user
// is leaving, so stepping out and back in lands where they were. Every content
// pane binds it to the same keys.
func (m *model) navBack() (tea.Model, tea.Cmd) {
	m.navFocus = true
	m.navCursor = int(m.section)
	return m, nil
}

// selectSection switches the active section, drops nav focus, and loads the
// section's data when entering it needs a fetch.
func (m *model) selectSection(s section) (tea.Model, tea.Cmd) {
	m.section = s
	m.navFocus = false
	m.navCursor = int(s)
	if enter := m.spec().enter; enter != nil {
		cmd := enter(m)
		return m, cmd
	}
	cmd := m.reload(s)
	return m, cmd
}

// reload fetches what a pane shows, on entry and on "r". The polled panes keep
// what the poller already fetched, so a pane shows current data and the
// loading state only before its first result.
func (m *model) reload(s section) tea.Cmd {
	if reload := sectionSpecs()[s].reload; reload != nil {
		return reload(m)
	}
	return nil
}

// moveCursor steps a menu's cursor for an up or down key, clamped to its n
// rows, and reports whether key was one.
func moveCursor(cursor *int, key string, n int) bool {
	switch key {
	case "up", "k":
		*cursor = max(*cursor-1, 0)
	case "down", "j":
		*cursor = max(min(*cursor+1, n-1), 0)
	default:
		return false
	}
	return true
}

// menuRow renders one menu row, with a ">" marker on the active one.
func menuRow(label string, active bool) string {
	if active {
		return navActiveStyle.Render("> " + label)
	}
	return navItemStyle.Render("  " + label)
}

// handleNavKey drives the focused sidebar: move the highlight, open a section,
// or drop focus back to the content.
func (m *model) handleNavKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if moveCursor(&m.navCursor, key.String(), int(sectionCount)) {
		return m, nil
	}
	switch key.String() {
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
	for i, spec := range sectionSpecs() {
		active := (!m.navFocus && section(i) == m.section) || (m.navFocus && i == m.navCursor)
		b.WriteString(menuRow(spec.label, active))
		b.WriteString("\n")
	}

	// Persistent DB-health indicator, below a divider, visible from any section.
	b.WriteString(navDisabledStyle.Render(strings.Repeat("-", sidebarWidth-2)))
	b.WriteString("\n")
	b.WriteString(m.dbIndicator())

	divider := activeTheme.border
	if m.navFocus {
		divider = activeTheme.accent
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

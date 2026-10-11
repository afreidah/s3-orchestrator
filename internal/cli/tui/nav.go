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

// section is a top-level nav destination. The zero value is the Dashboard, so
// a new model opens on it.
type section int

const (
	sectionDashboard section = iota
	sectionFiles
	sectionBackends
	sectionBuckets
	sectionReplication
	sectionWorkers
	sectionCleanup
	sectionCache
	sectionConfig
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
		{"Dashboard", sectionDashboard, true},
		{"Files", sectionFiles, true},
		{"Backends", sectionBackends, true},
		{"Buckets", sectionBuckets, true},
		{"Replication", sectionReplication, true},
		{"Workers", sectionWorkers, true},
		{"Cleanup", sectionCleanup, true},
		{"Cache", sectionCache, true},
		{"Config", sectionConfig, true},
		{"Logs", sectionLogs, true},
		{"Ops", sectionOps, true},
	}
}

// selectableSections is the number of enabled nav destinations; it bounds the
// nav cursor.
const selectableSections = 11

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
	switch s {
	// The polled panes keep what the poller already fetched, so a pane opens on
	// current data and shows the loading state only before its first result.
	case sectionDashboard:
		cmd := m.refreshDashboard()
		return m, cmd
	case sectionBackends:
		m.backends.loading = !m.backends.list.loaded()
		cmd := m.fetch(pollStatus)
		return m, cmd
	case sectionBuckets:
		m.buckets.loading = !m.buckets.list.loaded()
		cmd := m.fetch(pollBuckets)
		return m, cmd
	case sectionReplication:
		return m.enterReplication()
	case sectionWorkers:
		m.workers.loading = !m.workers.list.loaded()
		cmd := m.fetch(pollWorkers)
		return m, cmd
	case sectionCleanup:
		m.cleanup.loading = !m.cleanup.loaded
		cmd := m.fetch(pollCleanup)
		return m, cmd
	case sectionCache:
		m.cache.loading = m.cache.snap == nil
		cmd := m.fetch(pollCache)
		return m, cmd
	case sectionOps:
		// Entering from the nav is always the fleet-wide menu. A backend-scoped
		// one is opened by the backends pane, which fills these in itself.
		m.ops = opsView{actions: opsActions()}
		return m, nil
	case sectionConfig:
		m.config.loading = m.config.resp == nil
		cmd := m.loadConfig()
		return m, cmd
	case sectionLogs:
		m.logs = newLogsView()
		m.logs.loading = true
		cmd := m.fetch(pollLogs)
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

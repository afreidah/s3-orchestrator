// -------------------------------------------------------------------------------
// TUI - Styles
//
// Author: Alex Freidah
//
// Lipgloss styles for the browser view. Colours use 256-colour codes; Lipgloss
// degrades them automatically on terminals with fewer colours.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	// titleStyle renders the app title bar of the pane that has focus.
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62")).Padding(0, 1)

	// titleMutedStyle renders the title bar of the pane that does not have
	// focus, so the focused pane is obvious at a glance.
	titleMutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Background(lipgloss.Color("238")).Padding(0, 1)

	// pathStyle renders the current prefix and the empty-listing notice.
	pathStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

	// selectedStyle renders the table's highlighted row.
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))

	// helpStyle renders the footer key hints.
	helpStyle = lipgloss.NewStyle().Faint(true)

	// colHeaderStyle renders a hand-rolled table's column header row.
	colHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("111"))

	// errStyle renders the error line.
	errStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))

	// confirmStyle renders an armed action's y/N confirmation prompt in the
	// footer; statusOKStyle and statusErrStyle render the action result.
	confirmStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("232")).Background(lipgloss.Color("214")).Padding(0, 1)
	statusOKStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("71"))
	statusErrStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))

	// sidebarStyle frames the left nav with a right divider.
	sidebarStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), false, true, false, false).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)

	// navTitleStyle renders the nav's app tag.
	navTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62")).Padding(0, 1)

	// navItemStyle, navActiveStyle, and navDisabledStyle render nav entries by
	// state: idle, current/highlighted, and not-yet-available.
	navItemStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	navActiveStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	navDisabledStyle = lipgloss.NewStyle().Faint(true)

	// logLevelStyles colours the logs pane's LEVEL column by severity. INFO
	// (the bulk of entries) stays neutral so WARN and ERROR stand out rather
	// than drowning in colour; DEBUG is dimmed. Unknown levels fall back to
	// logLevelInfo.
	logLevelDebug = lipgloss.NewStyle().Faint(true)
	logLevelInfo  = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	logLevelWarn  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	logLevelError = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))
)

// levelStyle normalizes a log-level string and returns its display style.
func levelStyle(level string) lipgloss.Style {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "DEBUG":
		return logLevelDebug
	case "WARN", "WARNING":
		return logLevelWarn
	case "ERROR":
		return logLevelError
	default:
		return logLevelInfo
	}
}

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

// -------------------------------------------------------------------------
// PANE AND TABLE STYLES
// -------------------------------------------------------------------------

// titleStyle and the other styles the browser pane draws itself with.
//
// The muted title is what makes focus obvious: the pane holding focus keeps the
// bright bar, the other drops to grey. The tag label matches the column headers
// so it reads as a field name, and its left pad aligns the line with the padded
// table cells beneath it.
var (
	titleStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62")).Padding(0, 1)
	titleMutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Background(lipgloss.Color("238")).Padding(0, 1)
	pathStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("245")) // current prefix and the empty-listing notice
	selectedStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))
	helpStyle       = lipgloss.NewStyle().Faint(true)                                  // footer key hints
	colHeaderStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("111")) // column header row
	tagLabelStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("111")).PaddingLeft(1)
	tagValueStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	errStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))
)

// -------------------------------------------------------------------------
// ACTION FEEDBACK
// -------------------------------------------------------------------------

// confirmStyle renders an armed action's y/N prompt in the footer; the status
// styles render what the action reported back.
var (
	confirmStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("232")).Background(lipgloss.Color("214")).Padding(0, 1)
	statusOKStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("71"))
	statusErrStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))
)

// -------------------------------------------------------------------------
// NAVIGATION
// -------------------------------------------------------------------------

// sidebarStyle frames the left nav with a right divider; the item styles render
// its entries by state.
var (
	sidebarStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), false, true, false, false).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
	navTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62")).Padding(0, 1)
	navItemStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("252")) // idle
	navActiveStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	navDisabledStyle = lipgloss.NewStyle().Faint(true) // not yet available
)

// -------------------------------------------------------------------------
// LOG LEVELS
// -------------------------------------------------------------------------

// logLevelDebug and friends colour the logs pane's LEVEL column by severity.
// INFO carries the bulk of the entries and stays neutral so WARN and ERROR
// stand out rather than drowning in colour. Unknown levels fall back to info.
var (
	logLevelDebug = lipgloss.NewStyle().Faint(true)
	logLevelInfo  = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	logLevelWarn  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	logLevelError = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))
)

// -------------------------------------------------------------------------
// SELECTORS
// -------------------------------------------------------------------------

// usageStyle colours a usage percentage: green under 70, yellow through 90,
// red at or above 90, so a near-full backend stands out.
func usageStyle(pct int) lipgloss.Style {
	switch {
	case pct >= 90:
		return statusErrStyle
	case pct >= 70:
		return logLevelWarn
	default:
		return statusOKStyle
	}
}

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

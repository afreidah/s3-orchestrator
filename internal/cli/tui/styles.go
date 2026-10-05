// -------------------------------------------------------------------------------
// TUI - Styles
//
// Author: Alex Freidah
//
// Lipgloss styles for every pane, built from the active theme. useTheme sets
// them once at startup, before the program draws anything, so the render
// code reads them as fixed values. Lipgloss degrades the colours on
// terminals with fewer of them.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// activeTheme is the theme the styles were last built from. The sidebar
// reads its border colours from it directly, because the divider changes
// colour with focus.
var activeTheme theme

// titleStyle and the other styles the panes draw themselves with.
//
// The muted title is what makes focus obvious: the pane holding focus keeps the
// bright bar, the other drops to the surface colour. The tag label matches the
// column headers so it reads as a field name, and its left pad aligns the line
// with the padded table cells beneath it. helpStyle, navDisabledStyle and
// logLevelDebug are faint rather than coloured, so they recede on any theme.
var (
	titleStyle       lipgloss.Style
	titleMutedStyle  lipgloss.Style
	pathStyle        lipgloss.Style
	selectedStyle    lipgloss.Style
	helpStyle        lipgloss.Style
	colHeaderStyle   lipgloss.Style
	tagLabelStyle    lipgloss.Style
	tagValueStyle    lipgloss.Style
	errStyle         lipgloss.Style
	confirmStyle     lipgloss.Style
	statusOKStyle    lipgloss.Style
	statusErrStyle   lipgloss.Style
	sidebarStyle     lipgloss.Style
	navTitleStyle    lipgloss.Style
	navItemStyle     lipgloss.Style
	navActiveStyle   lipgloss.Style
	navDisabledStyle lipgloss.Style
	logLevelDebug    lipgloss.Style
	logLevelInfo     lipgloss.Style
	logLevelWarn     lipgloss.Style
	logLevelError    lipgloss.Style
)

func init() {
	def := themePresets[defaultThemeName]
	useTheme(&def)
}

// useTheme builds every style from t. It is called once at startup; nothing
// renders concurrently with it.
func useTheme(t *theme) {
	activeTheme = *t
	plain := lipgloss.NewStyle()
	bold := plain.Bold(true)

	titleStyle = bold.Foreground(t.titleFG).Background(t.titleBG).Padding(0, 1)
	titleMutedStyle = plain.Foreground(t.muted).Background(t.surface).Padding(0, 1)
	pathStyle = plain.Foreground(t.muted)
	selectedStyle = bold.Foreground(t.selectedFG).Background(t.selectedBG)
	helpStyle = plain.Faint(true)
	colHeaderStyle = bold.Foreground(t.header)
	tagLabelStyle = bold.Foreground(t.header).PaddingLeft(1)
	tagValueStyle = plain.Foreground(t.text)
	errStyle = bold.Foreground(t.err)

	confirmStyle = bold.Foreground(t.titleFG).Background(t.warn).Padding(0, 1)
	statusOKStyle = bold.Foreground(t.ok)
	statusErrStyle = bold.Foreground(t.err)

	sidebarStyle = plain.Border(lipgloss.NormalBorder(), false, true, false, false).
		BorderForeground(t.border).Padding(0, 1)
	navTitleStyle = titleStyle
	navItemStyle = plain.Foreground(t.text)
	navActiveStyle = bold.Foreground(t.accent)
	navDisabledStyle = plain.Faint(true)

	logLevelDebug = plain.Faint(true)
	logLevelInfo = plain.Foreground(t.text)
	logLevelWarn = bold.Foreground(t.warn)
	logLevelError = bold.Foreground(t.err)
}

// usageStyle colours a usage percentage: ok under 70, warn through 90, error
// at or above 90, so a near-full backend stands out.
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

// -------------------------------------------------------------------------------
// TUI - Styles
//
// Author: Alex Freidah
//
// Lipgloss styles for the browser view. Colours use 256-colour codes; Lipgloss
// degrades them automatically on terminals with fewer colours.
// -------------------------------------------------------------------------------

package tui

import "github.com/charmbracelet/lipgloss"

var (
	// titleStyle renders the app title bar.
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62")).Padding(0, 1)

	// pathStyle renders the current prefix and the empty-listing notice.
	pathStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

	// selectedStyle renders the table's highlighted row.
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))

	// helpStyle renders the footer key hints.
	helpStyle = lipgloss.NewStyle().Faint(true)

	// errStyle renders the error line.
	errStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))
)

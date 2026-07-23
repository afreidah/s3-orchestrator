// -------------------------------------------------------------------------------
// TUI - Admin Action Framework
//
// Author: Alex Freidah
//
// The reusable plumbing shared by write actions: a y/N confirmation prompt, a
// transient result/status line, and the footer that renders whichever is
// active. A pane arms an action by building an adminAction (a confirm question
// plus the command to run on accept) and handing it to startAction; the Ops
// pane drives the actual operations and their streamed output.
// -------------------------------------------------------------------------------

package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// adminAction is a write operation the user can invoke. An empty confirm runs
// immediately; otherwise confirm is shown as a y/N prompt before run fires.
type adminAction struct {
	confirm string
	run     tea.Cmd
}

// confirmPrompt is the pending confirmation: the question and the command to
// run when the user accepts.
type confirmPrompt struct {
	text string
	run  tea.Cmd
}

// actionStatus is the transient result of the last action, shown in the footer
// until the next keypress.
type actionStatus struct {
	ok   bool
	text string
}

// startAction runs an action immediately, or arms a confirmation when the
// action requires one.
func (m *model) startAction(a adminAction) (tea.Model, tea.Cmd) {
	if a.confirm != "" {
		m.confirm = &confirmPrompt{text: a.confirm, run: a.run}
		return m, nil
	}
	return m, a.run
}

// handleConfirmKey resolves a keypress while a confirmation is armed: accept
// (y/enter) runs the pending command, anything else cancels.
func (m *model) handleConfirmKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "y", "Y", "enter":
		run := m.confirm.run
		m.confirm = nil
		return m, run
	default:
		m.confirm = nil
		return m, nil
	}
}

// footer renders the content pane's bottom line: a confirmation prompt or the
// last action's result take priority over the pane's key hints. Centralised so
// every pane shares one confirm/status surface.
func (m *model) footer(hints string) string {
	switch {
	case m.confirm != nil:
		return confirmStyle.Width(m.contentWidth()).Render(m.confirm.text + "  (y/N)")
	case m.status != nil:
		style := statusOKStyle
		if !m.status.ok {
			style = statusErrStyle
		}
		return style.Width(m.contentWidth()).Render(m.status.text)
	default:
		return helpStyle.Width(m.contentWidth()).Render(hints)
	}
}

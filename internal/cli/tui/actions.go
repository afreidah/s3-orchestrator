// -------------------------------------------------------------------------------
// TUI - Admin Action Framework
//
// Author: Alex Freidah
//
// The reusable plumbing behind write actions: a confirmation prompt, async
// execution, and a result/status line. A pane triggers an action by building an
// adminAction (an optional confirm question plus a command that performs the
// work and returns an actionResultMsg) and handing it to startAction. Adding a
// new admin function then costs a client method, a run command, and one key
// case - the confirm UI, result display, and pane refresh are all handled here.
// -------------------------------------------------------------------------------

package tui

import (
	"context"

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

// actionResultMsg is delivered when an action's command completes.
type actionResultMsg struct {
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

// runAction wraps a client call as a command that reports success or failure as
// an actionResultMsg. name is used in the status line ("<name> done" / "<name>
// failed: ...").
func (m *model) runAction(name string, fn func(context.Context) error) tea.Cmd {
	return func() tea.Msg {
		if err := fn(context.Background()); err != nil {
			return actionResultMsg{ok: false, text: name + " failed: " + err.Error()}
		}
		return actionResultMsg{ok: true, text: name + " done"}
	}
}

// applyActionResult records the result and refreshes the active pane so it
// reflects any change the action made.
func (m *model) applyActionResult(msg actionResultMsg) (tea.Model, tea.Cmd) {
	m.status = &actionStatus{ok: msg.ok, text: msg.text}
	cmd := m.reloadCurrent()
	return m, cmd
}

// reloadCurrent returns a command that reloads whichever pane is active,
// marking it loading.
func (m *model) reloadCurrent() tea.Cmd {
	switch m.section {
	case sectionBackends:
		m.backends.loading = true
		return m.loadStatus()
	case sectionLogs:
		m.logs.loading = true
		return m.loadLogs()
	default:
		m.loading = true
		return m.loadObjects(m.prefix, "")
	}
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
		return helpStyle.Width(m.contentWidth()).Render(hints + "  -  R reconcile-usage  -  F flush-cache")
	}
}

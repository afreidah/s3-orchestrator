// -------------------------------------------------------------------------------
// TUI - Admin Action Framework
//
// Author: Alex Freidah
//
// The reusable plumbing shared by write actions: a y/N confirmation prompt, an
// input prompt for the actions that need a value before they can run, a
// transient result/status line, and the footer that renders whichever is
// active. A pane arms an action by building an adminAction (a confirm question
// plus the command to run on accept) and handing it to startAction; the Ops
// pane drives the actual operations and their streamed output.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// adminAction is a write operation the user can invoke. An empty confirm runs
// immediately; otherwise confirm is shown as a y/N prompt before run fires.
// before, when set, updates the pane the moment the action is accepted, so the
// user sees it start rather than waiting on the request that run dispatches.
type adminAction struct {
	confirm string
	before  func(*model)
	run     tea.Cmd
}

// confirmPrompt is the pending confirmation: the question, what to show on
// acceptance, and the command to run.
type confirmPrompt struct {
	text   string
	before func(*model)
	run    tea.Cmd
}

// actionStatus is the transient result of the last action, shown in the footer
// until the next keypress.
type actionStatus struct {
	ok   bool
	text string
}

// inputPrompt collects the one value an action needs before it can be armed:
// the key to invalidate, the prefix to sweep, the key id to rotate away from.
// build turns the typed value into the action that then runs, so an action
// that also confirms still passes through the same y/N gate afterwards.
type inputPrompt struct {
	text  string
	input textinput.Model
	build func(value string) adminAction
}

// askFor arms an input prompt. The action is not built until a value is
// submitted, so the question and the operation stay declared together at the
// call site.
func (m *model) askFor(question, placeholder string, build func(string) adminAction) (tea.Model, tea.Cmd) {
	in := textinput.New()
	in.Placeholder = placeholder
	in.Prompt = ""
	m.prompt = &inputPrompt{text: question, input: in, build: build}
	return m, m.prompt.input.Focus()
}

// handleInputKey drives an armed input prompt. Enter submits a non-empty value
// and arms whatever the action needs next; esc cancels. An empty submission is
// refused here rather than sent, since the endpoints that take a value reject
// an empty one on purpose.
func (m *model) handleInputKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc":
		m.prompt = nil
		return m, nil
	case "enter":
		value := strings.TrimSpace(m.prompt.input.Value())
		if value == "" {
			return m, nil
		}
		build := m.prompt.build
		m.prompt = nil
		return m.startAction(build(value))
	}

	var cmd tea.Cmd
	m.prompt.input, cmd = m.prompt.input.Update(key)
	return m, cmd
}

// startAction runs an action immediately, or arms a confirmation when the
// action requires one.
func (m *model) startAction(a adminAction) (tea.Model, tea.Cmd) {
	if a.confirm != "" {
		m.confirm = &confirmPrompt{text: a.confirm, before: a.before, run: a.run}
		return m, nil
	}
	m.begin(a.before)
	return m, a.run
}

// handleConfirmKey resolves a keypress while a confirmation is armed: accept
// (y/enter) runs the pending command, anything else cancels.
func (m *model) handleConfirmKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "y", "Y", "enter":
		before, run := m.confirm.before, m.confirm.run
		m.confirm = nil
		m.begin(before)
		return m, run
	default:
		m.confirm = nil
		return m, nil
	}
}

// begin applies an action's pane update on the main loop, before the command
// that does the work is dispatched. Running it here rather than inside the
// command is what makes the transition immediate: a command runs off the loop
// and cannot report anything until it returns.
func (m *model) begin(before func(*model)) {
	if before != nil {
		before(m)
	}
}

// footer renders the content pane's bottom line: a confirmation prompt or the
// last action's result take priority over the pane's key hints. Centralised so
// every pane shares one confirm/status surface.
func (m *model) footer(hints string) string {
	switch {
	case m.prompt != nil:
		return confirmStyle.Width(m.contentWidth()).
			Render(m.prompt.text + "  " + m.prompt.input.View() + "  (enter to run, esc to cancel)")
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

// -------------------------------------------------------------------------------
// TUI - Streamed Runs
//
// Author: Alex Freidah
//
// Running an admin action and streaming its output, for any pane. Accepting an
// action replaces the pane's content with a scrolling output viewport that
// renders exactly as the adminctl CLI does, including the live NDJSON progress
// stream the long-running actions emit; the short actions surface a single
// result line instead. Every action flows through one adminclient.EventStream
// so both render alike. One action runs at a time, shown by the pane that
// started it until the operator closes it.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// run is the one action streaming its output in the TUI. Only one runs at a
// time. The pane that started it shows the output in place of its own content
// until the operator closes it.
type run struct {
	owner   section                 // pane showing the output
	shown   bool                    // the owner is showing the output instead of its content
	running bool                    // the action is still streaming
	label   string                  // label of the action
	lines   []string                // rendered output lines
	pending string                  // step label awaiting its step_end (sequential ops)
	vp      viewport.Model          // scrolling viewport over the output lines
	stream  adminclient.EventStream // live stream while running, nil when idle
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// runStreamMsg carries the opened action stream (or the error opening it).
type runStreamMsg struct {
	stream adminclient.EventStream
	err    error
	label  string
}

// runEventMsg carries one streamed event.
type runEventMsg struct{ event adminstream.Event }

// runDoneMsg marks the stream exhausted; err is set on a read failure.
type runDoneMsg struct{ err error }

// openRun returns a command that starts an action off the main loop and reports
// the opened stream as a runStreamMsg.
func openRun(client adminClient, a *opsAction, req opsRequest) tea.Cmd {
	return func() tea.Msg {
		s, err := client.RunOp(context.Background(), a, req)
		return runStreamMsg{stream: s, err: err, label: a.label}
	}
}

// readRun returns a command that pulls the next event off the stream, reporting
// a runEventMsg or, at the end, a runDoneMsg.
func readRun(s adminclient.EventStream) tea.Cmd {
	return func() tea.Msg {
		e, err := s.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return runDoneMsg{}
			}
			return runDoneMsg{err: err}
		}
		return runEventMsg{event: e}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// startRun confirms an action and streams it into the current pane. A second
// action is refused while one is running.
func (m *model) startRun(a *opsAction, req opsRequest) (tea.Model, tea.Cmd) {
	if m.runBusy() {
		return m, nil
	}
	return m.startAction(m.runAction(a, req))
}

// runBusy reports whether an action is already running, and says so in the
// footer, so a pane does not start or prompt for a second one.
func (m *model) runBusy() bool {
	if !m.run.running {
		return false
	}
	m.status = &actionStatus{text: "an action is already running; wait for it to finish"}
	return true
}

// runAction arms one action against the request it resolved to, so a prompted
// action and a bare one reach the same confirm-and-run path. A run scoped to
// one backend names it in its label, so it cannot be read as the fleet-wide
// pass of the same name.
func (m *model) runAction(a *opsAction, req opsRequest) adminAction {
	label := a.label
	if backend := req.query.Get(queryBackend); backend != "" {
		label += " on " + backend
	}
	return adminAction{
		confirm: a.confirm,
		before:  func(m *model) { m.beginRun(label) },
		run:     openRun(m.client, a, req),
	}
}

// beginRun clears the output and shows the action as running in the current
// pane. Called the moment the action is accepted, so a long operation reports
// that it started instead of leaving the pane live until the request returns.
func (m *model) beginRun(label string) {
	m.run.owner = m.section
	m.run.shown = true
	m.run.running = true
	m.run.label = label
	m.run.lines = nil
	m.run.pending = ""
	m.run.stream = nil
	m.run.vp.SetContent("")
	m.appendRunLine(pathStyle.Render("running " + label + "..."))
}

// applyRunStream begins reading the opened stream, or records the failure to
// start the action. The pane already switched to output when the action was
// accepted.
func (m *model) applyRunStream(msg runStreamMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.run.running = false
		m.run.stream = nil
		m.appendRunLine(errStyle.Render("error: " + msg.err.Error()))
		m.report(false, msg.label+" failed")
		return m, nil
	}
	m.run.stream = msg.stream
	return m, readRun(msg.stream)
}

// applyRunEvent renders one event and continues reading the stream.
func (m *model) applyRunEvent(e *adminstream.Event) (tea.Model, tea.Cmd) {
	if line := m.runEventLine(e); line != "" {
		m.appendRunLine(line)
	}
	if e.Kind == adminstream.KindResult {
		ok := e.Outcome != adminstream.OutcomeFailed
		m.report(ok, m.run.label+": "+e.Outcome)
	}
	return m, readRun(m.run.stream)
}

// applyRunDone finalizes a completed action, closing the stream.
func (m *model) applyRunDone(msg runDoneMsg) (tea.Model, tea.Cmd) {
	if m.run.stream != nil {
		_ = m.run.stream.Close()
		m.run.stream = nil
	}
	m.run.running = false
	if msg.err != nil {
		m.appendRunLine(errStyle.Render("stream error: " + msg.err.Error()))
		m.report(false, m.run.label+" failed")
	}
	return m, nil
}

// showingRun reports whether the current pane is showing the run's output in
// place of its own content.
func (m *model) showingRun() bool {
	return m.run.shown && m.run.owner == m.section
}

// handleRunKey drives the output: while the action runs only scrolling is
// allowed; once it finishes, esc/enter closes the output and the pane's own
// content returns.
func (m *model) handleRunKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if !m.run.running {
		switch key.String() {
		case "esc", "left", "h", "enter":
			m.run.shown = false
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.run.vp, cmd = m.run.vp.Update(key)
	return m, cmd
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// appendRunLine adds a line to the output and scrolls to the bottom.
func (m *model) appendRunLine(line string) {
	m.run.lines = append(m.run.lines, line)
	m.run.vp.SetContent(strings.Join(m.run.lines, "\n"))
	m.run.vp.GotoBottom()
}

// runEventLine renders one event to a line, mirroring the CLI's progress output.
// A step_start records the pending label and emits nothing; its step_end
// completes the line.
func (m *model) runEventLine(e *adminstream.Event) string {
	switch e.Kind {
	case adminstream.KindStart:
		return e.Op + " started"
	case adminstream.KindProgress:
		if e.Message != "" {
			return "  " + e.Message
		}
		return fmt.Sprintf("  processed %d", e.Processed)
	case adminstream.KindStepStart:
		m.run.pending = e.Message
		return ""
	case adminstream.KindStepEnd:
		label := e.Message
		if label == "" {
			label = m.run.pending
		}
		m.run.pending = ""
		status := strings.ToUpper(e.Outcome)
		if status == "" {
			status = "OK"
		}
		return fmt.Sprintf("  %s ... %s", label, status)
	case adminstream.KindResult:
		return runResultLine(e)
	}
	return ""
}

// runResultLine renders the terminal result event.
func runResultLine(e *adminstream.Event) string {
	switch e.Outcome {
	case adminstream.OutcomeFailed:
		return errStyle.Render("error: " + e.Error)
	case adminstream.OutcomeSkipped:
		return "skipped: " + e.Message
	default:
		msg := e.Message
		if msg == "" {
			msg = fmt.Sprintf("processed %d", e.Processed)
		}
		return "done: " + msg
	}
}

// runPaneView composes the output's full-screen layout in the pane that
// started it: the pane and the action in the title, then the output.
func (m *model) runPaneView() string {
	state := "running"
	if !m.run.running {
		state = "done"
	}
	header := m.contentTitleStyle().Width(m.contentWidth()).
		Render(strings.ToLower(m.sectionLabel()) + "   " + m.run.label + "   " + state)
	return m.frame(header, m.hintFooter(), m.runBody())
}

// runBody renders the output, or a spinner until the first line arrives.
func (m *model) runBody() pane {
	if m.run.running && len(m.run.lines) == 0 {
		return textPane(m.spinner.View() + " starting...")
	}
	return m.viewportPane(&m.run.vp)
}

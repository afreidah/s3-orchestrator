// -------------------------------------------------------------------------------
// TUI - Ops View
//
// Author: Alex Freidah
//
// The admin instance-action pane: a menu of write operations, each armed with a
// y/N confirm and then run against the admin API. Accepting one switches to a
// scrolling output viewport that renders exactly as the adminctl CLI does,
// including the live NDJSON progress stream the long-running actions emit. The
// short actions surface a single result line instead. Every action flows through
// one adminclient.EventStream so both render alike.
// Reached with "o"; "esc" steps back to the menu, then to the nav.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// opsAction is one selectable admin operation. result decodes the single JSON
// summary a short action answers with; the long-running actions leave it nil
// and stream NDJSON progress instead. An action that needs a value from the
// operator sets ask, and resolve turns the typed value into the request that
// carries it.
type opsAction struct {
	label   string
	method  string
	path    string
	result  oneShotDecoder
	confirm string
	ask     inputRequest
	resolve func(value string) opsRequest
}

// inputRequest is the question an action asks before it can run, and the hint
// shown in the empty field.
type inputRequest struct {
	question    string
	placeholder string
}

// opsRequest is where an action sends and what it carries: the path (which a
// value can extend), the query, and the request body.
type opsRequest struct {
	path  string
	query url.Values
	body  []byte
}

// opsActions lists the instance actions in menu order: the maintenance passes
// first, then cache control, then the encryption transitions. Drain is
// intentionally excluded; it is a per-backend flow handled elsewhere.
func opsActions() []opsAction {
	return append(maintenanceActions(), append(cacheActions(), encryptionActions()...)...)
}

// opsOption adjusts an action that is not a plain POST-and-run.
type opsOption func(*opsAction)

// post builds an action that triggers a pass at path. result is nil for the
// operations that stream their own progress instead of answering with one
// summary.
func post(label, path, confirm string, result oneShotDecoder, opts ...opsOption) opsAction {
	a := opsAction{
		label:   label,
		method:  http.MethodPost,
		path:    path,
		confirm: confirm,
		result:  result,
	}
	for _, opt := range opts {
		opt(&a)
	}
	return a
}

// asks attaches an input prompt, so the value the operator types resolves into
// the request that carries it.
func asks(question, placeholder string, resolve func(string) opsRequest) opsOption {
	return func(a *opsAction) {
		a.ask = inputRequest{question: question, placeholder: placeholder}
		a.resolve = resolve
	}
}

// deletes sends the action as a DELETE, for the endpoints that drop something
// rather than start a pass.
func deletes() opsOption {
	return func(a *opsAction) { a.method = http.MethodDelete }
}

// maintenanceActions are the passes that keep placement, copy counts and
// integrity where the configuration says they should be.
func maintenanceActions() []opsAction {
	return []opsAction{
		post("Rebalance backends", "/admin/api/rebalance",
			"Rebalance objects across backends?", nil),
		post("Replicate under-replicated objects", "/admin/api/replicate",
			"Create the missing copies of under-replicated objects?", nil),
		post("Clean over-replicated copies", "/admin/api/over-replication",
			"Remove over-replicated object copies?", nil),
		post("Scrub (verify integrity)", "/admin/api/scrub",
			"Scrub every object to verify integrity?", nil),
		post("Backfill checksums", "/admin/api/backfill-checksums",
			"Backfill missing object checksums?", nil),
		post("Reconcile metadata", "/admin/api/reconcile",
			"Reconcile metadata against backends?", nil),
		post("Reconcile usage counters", "/admin/api/usage-reconcile",
			"Reconcile usage counters across all backends?", decodeOneShot[usageReconcileResult]),
		post("Flush usage counters to the database", "/admin/api/usage-flush",
			"Flush buffered usage counters to the database?", decodeOneShot[usageFlushResult]),
	}
}

// cacheActions drop cached object data. The two that take a key or a prefix
// ask for it instead of confirming: nothing is lost but a cached copy, and the
// value the operator typed is the statement of intent.
func cacheActions() []opsAction {
	return []opsAction{
		post("Flush object cache", "/admin/api/cache/flush",
			"Flush the in-memory object cache?", decodeOneShot[cacheFlushResult]),
		post("Invalidate one cached key", "/admin/api/cache/keys",
			"", decodeOneShot[cacheInvalidateKeyResult],
			deletes(),
			asks("Invalidate which key?", "bucket/path/object", func(value string) opsRequest {
				return opsRequest{path: "/admin/api/cache/keys/" + value}
			})),
		post("Invalidate a cached prefix", "/admin/api/cache/prefix",
			"", decodeOneShot[cacheFlushResult],
			deletes(),
			asks("Invalidate which prefix?", "bucket/path/", func(value string) opsRequest {
				return opsRequest{path: "/admin/api/cache/prefix", query: url.Values{"prefix": {value}}}
			})),
	}
}

// encryptionActions move stored objects between plaintext and ciphertext, or
// re-wrap the keys that seal them. Each confirmation says what the pass will
// read and rewrite, since these are metered fleet-wide operations rather than
// a setting being toggled.
func encryptionActions() []opsAction {
	return []opsAction{
		post("Encrypt existing objects", "/admin/api/encrypt-existing",
			"Read and rewrite every plaintext copy as ciphertext?", decodeOneShot[encryptExistingResult]),
		post("Decrypt existing objects", "/admin/api/decrypt-existing",
			"Read and rewrite every encrypted copy as plaintext?", decodeOneShot[decryptExistingResult]),
		post("Rotate encryption key", "/admin/api/rotate-encryption-key",
			"Re-wrap every object key sealed with that key id?", decodeOneShot[rotateKeyResult],
			asks("Rotate away from which key id?", "old key id", func(value string) opsRequest {
				body, _ := json.Marshal(adminapi.RotateEncryptionKeyRequest{OldKeyID: value})
				return opsRequest{path: "/admin/api/rotate-encryption-key", body: body}
			})),
	}
}

// opsView holds the state of the ops pane: the menu cursor and, once an action
// runs, the streamed output and its live stream.
type opsView struct {
	cursor  int                     // highlighted menu row
	showOut bool                    // showing the output pane instead of the menu
	running bool                    // an action is in flight
	label   string                  // label of the action currently shown
	lines   []string                // rendered output lines
	pending string                  // step label awaiting its step_end (sequential ops)
	vp      viewport.Model          // scrolling viewport over the output lines
	stream  adminclient.EventStream // live stream while running, nil when idle
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// opsStreamMsg carries the opened action stream (or the error opening it).
type opsStreamMsg struct {
	stream adminclient.EventStream
	err    error
	label  string
}

// opsEventMsg carries one streamed event.
type opsEventMsg struct{ event adminstream.Event }

// opsDoneMsg marks the stream exhausted; err is set on a read failure.
type opsDoneMsg struct{ err error }

// openOps returns a command that starts an action off the main loop and reports
// the opened stream as an opsStreamMsg.
func openOps(client adminClient, a *opsAction, req opsRequest) tea.Cmd {
	return func() tea.Msg {
		s, err := client.RunOp(context.Background(), a, req)
		return opsStreamMsg{stream: s, err: err, label: a.label}
	}
}

// readOps returns a command that pulls the next event off the stream, reporting
// an opsEventMsg or, at the end, an opsDoneMsg.
func readOps(s adminclient.EventStream) tea.Cmd {
	return func() tea.Msg {
		e, err := s.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return opsDoneMsg{}
			}
			return opsDoneMsg{err: err}
		}
		return opsEventMsg{event: e}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// enterOpsOutput clears the output pane and shows the action as running. Called
// the moment the action is accepted, so a long operation reports that it
// started instead of leaving the menu live until the request returns.
func (m *model) enterOpsOutput(label string) {
	m.ops.showOut = true
	m.ops.running = true
	m.ops.label = label
	m.ops.lines = nil
	m.ops.pending = ""
	m.ops.stream = nil
	m.ops.vp.SetContent("")
	m.appendOpsLine(pathStyle.Render("running " + label + "..."))
}

// applyOpsStream begins reading the opened stream, or records the failure to
// start the action. The pane already switched to output when the action was
// accepted.
func (m *model) applyOpsStream(msg opsStreamMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.ops.running = false
		m.ops.stream = nil
		m.appendOpsLine(errStyle.Render("error: " + msg.err.Error()))
		m.status = &actionStatus{ok: false, text: msg.label + " failed"}
		return m, nil
	}
	m.ops.stream = msg.stream
	return m, readOps(msg.stream)
}

// applyOpsEvent renders one event and continues reading the stream.
func (m *model) applyOpsEvent(e *adminstream.Event) (tea.Model, tea.Cmd) {
	if line := m.opsEventLine(e); line != "" {
		m.appendOpsLine(line)
	}
	if e.Kind == adminstream.KindResult {
		ok := e.Outcome != adminstream.OutcomeFailed
		m.status = &actionStatus{ok: ok, text: m.ops.label + ": " + e.Outcome}
	}
	return m, readOps(m.ops.stream)
}

// applyOpsDone finalizes a completed action, closing the stream.
func (m *model) applyOpsDone(msg opsDoneMsg) (tea.Model, tea.Cmd) {
	if m.ops.stream != nil {
		_ = m.ops.stream.Close()
		m.ops.stream = nil
	}
	m.ops.running = false
	if msg.err != nil {
		m.appendOpsLine(errStyle.Render("stream error: " + msg.err.Error()))
		m.status = &actionStatus{ok: false, text: m.ops.label + " failed"}
	}
	return m, nil
}

// handleOpsKey routes to the output or the menu depending on the current view.
func (m *model) handleOpsKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.ops.showOut {
		return m.handleOpsOutputKey(key)
	}
	switch key.String() {
	case "esc", "left", "h":
		m.navFocus = true
		m.navCursor = int(m.section)
		return m, nil
	case "up", "k":
		if m.ops.cursor > 0 {
			m.ops.cursor--
		}
		return m, nil
	case "down", "j":
		if m.ops.cursor < len(opsActions())-1 {
			m.ops.cursor++
		}
		return m, nil
	case "enter", "right", "l":
		actions := opsActions()
		a := &actions[m.ops.cursor]
		if a.ask.question != "" {
			return m.askFor(a.ask.question, a.ask.placeholder, func(value string) adminAction {
				return opsAdminAction(m.client, a, a.resolve(value))
			})
		}
		return m.startAction(opsAdminAction(m.client, a, opsRequest{path: a.path}))
	}
	return m, nil
}

// opsAdminAction arms one operation against the request it resolved to, so a
// prompted action and a bare one reach the same confirm-and-run path.
func opsAdminAction(client adminClient, a *opsAction, req opsRequest) adminAction {
	return adminAction{
		confirm: a.confirm,
		before:  func(m *model) { m.enterOpsOutput(a.label) },
		run:     openOps(client, a, req),
	}
}

// handleOpsOutputKey drives the output pane: while an action runs only scrolling
// is allowed; once it finishes, esc/enter returns to the menu.
func (m *model) handleOpsOutputKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if !m.ops.running {
		switch key.String() {
		case "esc", "left", "h", "enter":
			m.ops.showOut = false
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.ops.vp, cmd = m.ops.vp.Update(key)
	return m, cmd
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// appendOpsLine adds a line to the output and scrolls to the bottom.
func (m *model) appendOpsLine(line string) {
	m.ops.lines = append(m.ops.lines, line)
	m.ops.vp.SetContent(strings.Join(m.ops.lines, "\n"))
	m.ops.vp.GotoBottom()
}

// opsEventLine renders one event to a line, mirroring the CLI's progress output.
// A step_start records the pending label and emits nothing; its step_end
// completes the line.
func (m *model) opsEventLine(e *adminstream.Event) string {
	switch e.Kind {
	case adminstream.KindStart:
		return e.Op + " started"
	case adminstream.KindProgress:
		if e.Message != "" {
			return "  " + e.Message
		}
		return fmt.Sprintf("  processed %d", e.Processed)
	case adminstream.KindStepStart:
		m.ops.pending = e.Message
		return ""
	case adminstream.KindStepEnd:
		label := e.Message
		if label == "" {
			label = m.ops.pending
		}
		m.ops.pending = ""
		status := strings.ToUpper(e.Outcome)
		if status == "" {
			status = "OK"
		}
		return fmt.Sprintf("  %s ... %s", label, status)
	case adminstream.KindResult:
		return opsResultLine(e)
	}
	return ""
}

// opsResultLine renders the terminal result event.
func opsResultLine(e *adminstream.Event) string {
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

// resizeOps fits the output viewport to the window below the header and footer.
func (m *model) resizeOps() {
	m.ops.vp.Width = m.contentWidth()
	m.ops.vp.Height = max(m.height-3, 3)
}

// opsPaneView composes the pane's full-screen layout.
func (m *model) opsPaneView() string {
	return m.frame(m.opsHeaderView(), m.opsFooterView(), m.opsBodyView())
}

// opsHeaderView renders the title bar: the menu prompt, or the active action
// and its state while output is shown.
func (m *model) opsHeaderView() string {
	title := "ops   select an action"
	if m.ops.showOut {
		state := "running"
		if !m.ops.running {
			state = "done"
		}
		title = "ops   " + m.ops.label + "   " + state
	}
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title)
}

// opsFooterView renders the ops key hints for the current view.
func (m *model) opsFooterView() string {
	switch {
	case m.ops.showOut && m.ops.running:
		return m.footer("running - up/down scroll - q quit")
	case m.ops.showOut:
		return m.footer("up/down scroll - esc back - q quit")
	default:
		return m.footer("up/down move - enter run - tab nav - q quit")
	}
}

// opsBodyView renders the menu or the streamed output.
func (m *model) opsBodyView() string {
	if !m.ops.showOut {
		return m.opsMenuView()
	}
	if m.ops.running && len(m.ops.lines) == 0 {
		return m.spinner.View() + " starting..."
	}
	return m.ops.vp.View()
}

// opsMenuView renders the action list with the cursor marker.
func (m *model) opsMenuView() string {
	var b strings.Builder
	for i, a := range opsActions() {
		marker, style := "  ", navItemStyle
		if i == m.ops.cursor {
			marker, style = "> ", navActiveStyle
		}
		// A trailing ellipsis is the usual signal that a menu entry asks for
		// something before it acts, rather than acting on the spot.
		label := a.label
		if a.ask.question != "" {
			label += "..."
		}
		b.WriteString(style.Render(marker + label))
		b.WriteString("\n")
	}
	return b.String()
}

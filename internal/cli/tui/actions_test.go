// -------------------------------------------------------------------------------
// TUI - Admin Action Framework Tests
//
// Author: Alex Freidah
//
// Deterministic tests for the reusable action plumbing: confirm arming, confirm
// key resolution, result wrapping, the status-and-reload apply, the shared
// footer, and the instance-action key bindings.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestStartAction_ConfirmVsImmediate(t *testing.T) {
	t.Parallel()
	ran := false
	cmd := func() tea.Msg { ran = true; return nil }

	// With a confirm string, the action is armed, not run.
	m := initialModel(&fakeLister{})
	if _, got := m.startAction(adminAction{confirm: "Sure?", run: cmd}); got != nil {
		t.Error("armed action should not return a command")
	}
	if m.confirm == nil || m.confirm.text != "Sure?" {
		t.Errorf("confirm not armed: %+v", m.confirm)
	}

	// Without a confirm string, the action runs immediately.
	m2 := initialModel(&fakeLister{})
	if _, got := m2.startAction(adminAction{run: cmd}); got == nil {
		t.Fatal("immediate action should return its command")
	} else {
		got() // execute
	}
	if m2.confirm != nil || !ran {
		t.Errorf("immediate: confirm=%v ran=%v", m2.confirm, ran)
	}
}

func TestHandleConfirmKey_AcceptCancel(t *testing.T) {
	t.Parallel()
	arm := func() *model {
		m := initialModel(&fakeLister{})
		m.confirm = &confirmPrompt{text: "?", run: func() tea.Msg { return nil }}
		return m
	}

	// accept: y runs the pending command and clears the confirm.
	m := arm()
	_, cmd := m.handleConfirmKey(key("y"))
	if m.confirm != nil || cmd == nil {
		t.Errorf("accept: confirm=%v cmd=%v", m.confirm, cmd)
	}

	// cancel: any other key clears the confirm without running.
	m = arm()
	_, cmd = m.handleConfirmKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.confirm != nil || cmd != nil {
		t.Errorf("cancel: confirm=%v cmd=%v", m.confirm, cmd)
	}
}

func TestFooter_ConfirmStatusHints(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20

	// default: the pane hints are shown verbatim.
	if base := m.footer("move"); !strings.Contains(base, "move") {
		t.Errorf("hints footer missing %q: %q", "move", base)
	}
	// status takes priority over hints (both ok and error variants).
	m.status = &actionStatus{ok: true, text: "job done"}
	if got := m.footer("move"); !strings.Contains(got, "job done") || strings.Contains(got, "reconcile-usage") {
		t.Errorf("ok status footer = %q", got)
	}
	m.status = &actionStatus{ok: false, text: "job failed: boom"}
	if got := m.footer("move"); !strings.Contains(got, "job failed") {
		t.Errorf("err status footer = %q", got)
	}
	// confirm takes priority over everything.
	m.confirm = &confirmPrompt{text: "Sure?"}
	if got := m.footer("move"); !strings.Contains(got, "Sure?") || !strings.Contains(got, "y/N") {
		t.Errorf("confirm footer = %q", got)
	}
}

func TestHandleKey_DismissesStatusOnNextKey(t *testing.T) {
	t.Parallel()
	m := modelWith([]entry{{name: "a"}}, "", &fakeLister{})
	m.status = &actionStatus{ok: true, text: "done"}
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.status != nil {
		t.Error("a keypress should clear a lingering status line")
	}
}

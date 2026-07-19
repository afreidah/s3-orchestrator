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
		m.confirm = &confirmPrompt{text: "?", run: func() tea.Msg { return actionResultMsg{ok: true} }}
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

func TestRunAction_Result(t *testing.T) {
	t.Parallel()
	// success path
	ok := initialModel(&fakeLister{}).runAction("job", (&fakeLister{}).ReconcileUsage)()
	if msg, is := ok.(actionResultMsg); !is || !msg.ok || !strings.Contains(msg.text, "job done") {
		t.Errorf("success result = %#v", ok)
	}
	// failure path
	bad := initialModel(&fakeLister{}).runAction("job", errLister{}.ReconcileUsage)()
	if msg, is := bad.(actionResultMsg); !is || msg.ok || !strings.Contains(msg.text, "job failed") {
		t.Errorf("failure result = %#v", bad)
	}
}

func TestApplyActionResult_SetsStatusAndReloads(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "p/", &fakeLister{})
	_, cmd := m.applyActionResult(actionResultMsg{ok: true, text: "done"})
	if m.status == nil || !m.status.ok || m.status.text != "done" {
		t.Errorf("status = %+v", m.status)
	}
	if cmd == nil || !m.loading {
		t.Errorf("expected a reload cmd and loading; cmd=%v loading=%v", cmd, m.loading)
	}
}

func TestFooter_ConfirmStatusHints(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20

	// default: pane hints plus the instance-action hints.
	base := m.footer("move")
	for _, want := range []string{"move", "R reconcile-usage", "F flush-cache"} {
		if !strings.Contains(base, want) {
			t.Errorf("hints footer missing %q: %q", want, base)
		}
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

func TestReloadCurrent_PerSection(t *testing.T) {
	t.Parallel()
	// Backends section reloads status.
	mb := initialModel(&fakeLister{})
	mb.section = sectionBackends
	if cmd := mb.reloadCurrent(); cmd == nil || !mb.backends.loading {
		t.Errorf("backends: cmd=%v loading=%v", cmd, mb.backends.loading)
	}
	// Logs section reloads logs.
	ml := initialModel(&fakeLister{})
	ml.section = sectionLogs
	if cmd := ml.reloadCurrent(); cmd == nil || !ml.logs.loading {
		t.Errorf("logs: cmd=%v loading=%v", cmd, ml.logs.loading)
	}
	// Files (default) reloads the listing.
	mf := initialModel(&fakeLister{})
	if cmd := mf.reloadCurrent(); cmd == nil || !mf.loading {
		t.Errorf("files: cmd=%v loading=%v", cmd, mf.loading)
	}
}

func TestHandleKey_InstanceActionsArmConfirm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ key, want string }{
		{"R", "Reconcile usage"},
		{"F", "Flush the in-memory object cache"},
	} {
		m := modelWith(nil, "", &fakeLister{})
		m.handleKey(key(tc.key))
		if m.confirm == nil || !strings.Contains(m.confirm.text, tc.want) {
			t.Errorf("%q should arm confirm %q, got %+v", tc.key, tc.want, m.confirm)
		}
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

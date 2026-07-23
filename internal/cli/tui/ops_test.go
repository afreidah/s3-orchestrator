// -------------------------------------------------------------------------------
// TUI - Ops View Tests
//
// Author: Alex Freidah
//
// Covers the ops pane: menu navigation, arming an action's confirm, the stream
// open/event/done transitions for both a successful run and an open failure,
// per-event line rendering, and the menu/output view switch.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// TestOpsMenu_NavAndArm moves the cursor and arms the selected action's confirm.
func TestOpsMenu_NavAndArm(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionOps

	m.handleOpsKey(key("j")) // down to the second action
	if m.ops.cursor != 1 {
		t.Fatalf("cursor = %d, want 1", m.ops.cursor)
	}
	m.handleOpsKey(tea.KeyMsg{Type: tea.KeyEnter})
	want := opsActions()[1].confirm
	if m.ops.showOut {
		t.Error("arming a confirm should not switch to the output view")
	}
	if m.confirm == nil || m.confirm.text != want {
		t.Errorf("confirm = %+v, want %q", m.confirm, want)
	}
}

// TestOps_RunStreamsToCompletion drives an action's stream from open through the
// events to done, accumulating the rendered lines and the terminal status.
func TestOps_RunStreamsToCompletion(t *testing.T) {
	t.Parallel()
	events := []adminstream.Event{
		{Kind: adminstream.KindStart, Op: "scrub"},
		{Kind: adminstream.KindStepEnd, Message: "verifying a", Outcome: adminstream.OutcomeOK, DurationMs: 3},
		{Kind: adminstream.KindResult, Outcome: adminstream.OutcomeOK, Message: "2 checked"},
	}
	m := initialModel(&fakeLister{opEvents: events})
	m.width, m.height = 100, 20
	m.section = sectionOps
	m.resizeOps()

	// open: switches to the output view and starts reading.
	if _, cmd := m.applyOpsStream(opsStreamMsg{stream: &sliceStream{events: events}, label: "Scrub"}); cmd == nil {
		t.Fatal("open should return a read command")
	}
	if !m.ops.showOut || !m.ops.running {
		t.Fatalf("after open: showOut=%v running=%v", m.ops.showOut, m.ops.running)
	}
	// feed each event.
	for _, e := range events {
		m.applyOpsEvent(&e)
	}
	if m.status == nil || !m.status.ok || !strings.Contains(m.status.text, "ok") {
		t.Errorf("terminal status = %+v", m.status)
	}
	// done: stops running.
	m.applyOpsDone(opsDoneMsg{})
	if m.ops.running {
		t.Error("done should clear running")
	}
	joined := strings.Join(m.ops.lines, "\n")
	for _, want := range []string{"scrub started", "verifying a", "OK", "done: 2 checked"} {
		if !strings.Contains(joined, want) {
			t.Errorf("output %q missing %q", joined, want)
		}
	}
}

// TestOps_OpenError surfaces a failure to open the action as an error line and a
// failed status, without leaving the pane running.
func TestOps_OpenError(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 100, 20
	m.applyOpsStream(opsStreamMsg{err: errNope, label: "Rebalance"})
	if m.ops.running {
		t.Error("open error should not leave the pane running")
	}
	if m.status == nil || m.status.ok || !strings.Contains(m.status.text, "Rebalance failed") {
		t.Errorf("status = %+v", m.status)
	}
	if !strings.Contains(strings.Join(m.ops.lines, "\n"), "nope") {
		t.Errorf("expected an error line, got %v", m.ops.lines)
	}
}

// TestOps_EventLines renders each event kind, including the sequential
// step_start/step_end pairing.
func TestOps_EventLines(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	// a sequential step: step_start records the label, step_end completes it.
	if got := m.opsEventLine(&adminstream.Event{Kind: adminstream.KindStepStart, Message: "hashing k"}); got != "" {
		t.Errorf("step_start should emit no line, got %q", got)
	}
	if got := m.opsEventLine(&adminstream.Event{Kind: adminstream.KindStepEnd, Outcome: adminstream.OutcomeOK}); !strings.Contains(got, "hashing k") || !strings.Contains(got, "OK") {
		t.Errorf("step_end line = %q", got)
	}
	if got := m.opsEventLine(&adminstream.Event{Kind: adminstream.KindProgress, Processed: 5}); !strings.Contains(got, "processed 5") {
		t.Errorf("progress line = %q", got)
	}
	if got := opsResultLine(&adminstream.Event{Kind: adminstream.KindResult, Outcome: adminstream.OutcomeFailed, Error: "boom"}); !strings.Contains(got, "boom") {
		t.Errorf("failed result line = %q", got)
	}
	if got := opsResultLine(&adminstream.Event{Kind: adminstream.KindResult, Outcome: adminstream.OutcomeSkipped, Message: "factor 1"}); !strings.Contains(got, "skipped") {
		t.Errorf("skipped result line = %q", got)
	}
}

// TestOps_OutputBackToMenu returns from a finished run's output to the menu.
func TestOps_OutputBackToMenu(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionOps
	m.ops.showOut = true
	m.ops.running = false
	m.handleOpsKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.ops.showOut {
		t.Error("esc on a finished run should return to the menu")
	}
}

// -------------------------------------------------------------------------------
// TUI - Run a Worker's Ops Action Tests
//
// Author: Alex Freidah
//
// Covers R on the Workers pane: it runs the Ops action that does the
// highlighted worker's job and streams it in the Workers pane, refuses a
// second action while one is running, closes the output with esc once it has
// finished, and does nothing for a worker with no Ops action.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"

	tea "github.com/charmbracelet/bubbletea"
)

// workersWithScrubber returns a model on the Workers pane with the cursor on
// the scrubber.
func workersWithScrubber(f *fakeLister) *model {
	m := initialModel(f)
	m.width, m.height = 120, 20
	m.section = sectionWorkers
	m.applyWorkers(&adminapi.WorkersResponse{Workers: []adminapi.WorkerHealth{{Name: "cleanup_queue"}, {Name: "scrubber"}}})
	m.workers.list.table.SetCursor(1)
	return m
}

// TestRunWorkerOpsAction_StreamsInTheWorkersPane verifies R confirms with the
// Scrub action's own text, streams it in the Workers pane, and esc closes the
// output once it has finished.
func TestRunWorkerOpsAction_StreamsInTheWorkersPane(t *testing.T) {
	t.Parallel()
	f := &fakeLister{opEvents: []adminstream.Event{
		{Kind: adminstream.KindStart, Op: "scrub"},
		{Kind: adminstream.KindStepEnd, Message: "verifying photos/a.jpg", Outcome: adminstream.OutcomeOK},
		{Kind: adminstream.KindResult, Outcome: adminstream.OutcomeOK, Message: "1 checked"},
	}}
	m := workersWithScrubber(f)

	m.handleKey(key("R"))
	if m.section != sectionWorkers || m.confirm == nil || !strings.Contains(m.confirm.text, "Scrub") {
		t.Fatalf("section=%d confirm=%+v, want the Scrub confirmation in Workers", m.section, m.confirm)
	}
	msg := accept(t, m)
	if f.opRequest.path != pathScrub {
		t.Errorf("request path = %q, want %q", f.opRequest.path, pathScrub)
	}
	_, cmd := m.Update(msg)
	for cmd != nil {
		_, cmd = m.Update(cmd())
	}

	view := m.contentView()
	for _, want := range []string{"workers", "verifying photos/a.jpg ... OK", "done: 1 checked"} {
		if !strings.Contains(view, want) {
			t.Errorf("Workers pane missing %q:\n%s", want, view)
		}
	}

	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.showingRun() || m.section != sectionWorkers {
		t.Errorf("after esc: showing run=%v section=%d, want the worker list back", m.showingRun(), m.section)
	}
}

// TestRunWorkerOpsAction_OneAtATime verifies a second action is refused while
// one is running.
func TestRunWorkerOpsAction_OneAtATime(t *testing.T) {
	t.Parallel()
	m := workersWithScrubber(&fakeLister{})
	m.run.running = true

	m.handleKey(key("R"))
	if m.confirm != nil || m.status == nil || !strings.Contains(m.status.text, "already running") {
		t.Errorf("confirm=%+v status=%+v, want the busy notice and nothing armed", m.confirm, m.status)
	}
}

// TestRunWorkerOpsAction_NoMatchingAction verifies R does nothing for a
// worker no Ops action runs.
func TestRunWorkerOpsAction_NoMatchingAction(t *testing.T) {
	t.Parallel()
	m := workersWithScrubber(&fakeLister{})
	m.workers.list.table.SetCursor(0)

	m.handleKey(key("R"))
	if m.confirm != nil {
		t.Errorf("confirm = %+v, want nothing armed for cleanup_queue", m.confirm)
	}
}

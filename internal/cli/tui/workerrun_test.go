// -------------------------------------------------------------------------------
// TUI - Run a Worker Tests
//
// Author: Alex Freidah
//
// Covers running a worker from the Workers pane: R confirms against the
// highlighted worker, the run streams into the ops output pane under the
// workers heading, and esc after it finishes returns to the Workers pane.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"

	tea "github.com/charmbracelet/bubbletea"
)

// TestRunWorker_StreamsIntoTheOutputPane drives a run from the Workers pane
// through to its result and back.
func TestRunWorker_StreamsIntoTheOutputPane(t *testing.T) {
	t.Parallel()
	f := &fakeLister{opEvents: []adminstream.Event{
		{Kind: adminstream.KindStart, Op: "run replication"},
		{Kind: adminstream.KindProgress, Message: "copied object key=photos/a.jpg"},
		{Kind: adminstream.KindResult, Outcome: adminstream.OutcomeOK},
	}}
	m := initialModel(f)
	m.width, m.height = 120, 20
	m.section = sectionWorkers
	m.applyWorkers(&adminapi.WorkersResponse{Workers: []adminapi.WorkerHealth{{Name: "scrubber"}, {Name: "replication"}}})
	m.workers.list.table.SetCursor(1)

	m.handleKey(key("R"))
	if m.confirm == nil || !strings.Contains(m.confirm.text, "replication") {
		t.Fatalf("confirm = %+v, want one naming replication", m.confirm)
	}
	msg := accept(t, m)
	if m.section != sectionOps || m.ops.worker != "replication" || !m.ops.running {
		t.Fatalf("after accepting: section=%d worker=%q running=%v", m.section, m.ops.worker, m.ops.running)
	}
	if f.opRequest.path != "/admin/api/workers/replication/run" {
		t.Errorf("request path = %q", f.opRequest.path)
	}

	_, cmd := m.Update(msg)
	for cmd != nil {
		var next tea.Cmd
		_, next = m.Update(cmd())
		cmd = next
	}
	view := m.contentView()
	for _, want := range []string{"workers", "run replication", "copied object key=photos/a.jpg", "done"} {
		if !strings.Contains(view, want) {
			t.Errorf("output missing %q:\n%s", want, view)
		}
	}

	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.section != sectionWorkers {
		t.Errorf("esc after the run went to section %d, want Workers", m.section)
	}
}

// TestRunWorker_NothingSelected verifies R does nothing on an empty list.
func TestRunWorker_NothingSelected(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionWorkers
	m.handleKey(key("R"))
	if m.confirm != nil {
		t.Error("R armed a run with no worker selected")
	}
}

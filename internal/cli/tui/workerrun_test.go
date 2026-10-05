// -------------------------------------------------------------------------------
// TUI - Run a Worker's Ops Action Tests
//
// Author: Alex Freidah
//
// Covers R on the Workers pane: it opens the Ops pane on the action that does
// the highlighted worker's job and runs it the way the Ops menu does, and
// does nothing for a worker with no Ops action.
// -------------------------------------------------------------------------------

package tui

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// TestRunWorkerOpsAction_RunsTheMatchingOpsAction verifies R on the scrubber
// lands on the Ops pane with the Scrub action's confirmation armed, and that
// accepting it sends the Scrub request.
func TestRunWorkerOpsAction_RunsTheMatchingOpsAction(t *testing.T) {
	t.Parallel()
	f := &fakeLister{}
	m := initialModel(f)
	m.section = sectionWorkers
	m.applyWorkers(&adminapi.WorkersResponse{Workers: []adminapi.WorkerHealth{{Name: "cleanup_queue"}, {Name: "scrubber"}}})
	m.workers.list.table.SetCursor(1)

	m.handleKey(key("R"))
	if m.section != sectionOps || m.ops.actions[m.ops.cursor].path != pathScrub {
		t.Fatalf("section=%d action=%q, want the Ops pane on Scrub", m.section, m.ops.actions[m.ops.cursor].path)
	}
	accept(t, m)
	if f.opRequest.path != pathScrub {
		t.Errorf("request path = %q, want %q", f.opRequest.path, pathScrub)
	}
}

// TestRunWorkerOpsAction_NoMatchingAction verifies R does nothing for a
// worker no Ops action runs.
func TestRunWorkerOpsAction_NoMatchingAction(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionWorkers
	m.applyWorkers(&adminapi.WorkersResponse{Workers: []adminapi.WorkerHealth{{Name: "cleanup_queue"}}})

	m.handleKey(key("R"))
	if m.section != sectionWorkers || m.confirm != nil {
		t.Errorf("section=%d confirm=%+v, want nothing to happen", m.section, m.confirm)
	}
}

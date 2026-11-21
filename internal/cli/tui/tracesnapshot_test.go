// -------------------------------------------------------------------------------
// TUI - Trace Snapshot Tests
//
// Covers the Ops action that saves the flight-recorder snapshot: it is offered
// in the fleet menu, asks for a destination, and writes the body to that file
// whole through the same path object downloads use.
// -------------------------------------------------------------------------------

package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestOpsMenu_TraceSnapshotSavesFile drives the trace snapshot entry from the
// fleet menu through its destination prompt and asserts the file holds the
// snapshot bytes once the transfer finishes.
func TestOpsMenu_TraceSnapshotSavesFile(t *testing.T) {
	t.Parallel()
	f := &fakeLister{payload: []byte("trace-bytes")}
	m := initialModel(f)
	m.section = sectionOps
	m.ops.actions = opsActions()
	m.ops.cursor = len(m.ops.actions) - 1

	m.handleOpsKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.prompt == nil {
		t.Fatal("selecting the trace snapshot should ask for a file")
	}
	if got := m.prompt.input.Placeholder; got != "trace.bin" {
		t.Errorf("prompt placeholder = %q, want %q", got, "trace.bin")
	}

	dest := filepath.Join(t.TempDir(), "snap.bin")
	m.prompt.input.SetValue(dest)
	m.handleInputKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.transfer == nil {
		t.Fatal("submitting a file should start the snapshot download")
	}
	settle(t, m)

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading saved snapshot: %v", err)
	}
	if string(got) != "trace-bytes" {
		t.Errorf("saved snapshot = %q, want %q", got, "trace-bytes")
	}
}

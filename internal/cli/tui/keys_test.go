// -------------------------------------------------------------------------------
// TUI - Keymap, Help and Action Log Tests
//
// Author: Alex Freidah
//
// Covers the footer showing each pane's labelled keys, the help overlay
// opening on "?" and closing on any key, and the session action log keeping
// every result after the footer has cleared it.
// -------------------------------------------------------------------------------

package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	tea "github.com/charmbracelet/bubbletea"
)

// TestHintLine_EveryPane verifies every pane's footer ends with help and quit
// and carries a key from its own keymap.
func TestHintLine_EveryPane(t *testing.T) {
	t.Parallel()
	want := map[section]string{
		sectionDashboard:   "r reload",
		sectionFiles:       "/ filter",
		sectionBackends:    "d drain",
		sectionBuckets:     "s sort",
		sectionReplication: "r reload",
		sectionWorkers:     "s sort",
		sectionCleanup:     "t dead-letter",
		sectionCache:       "r reload",
		sectionLogs:        "L level",
		sectionOps:         "enter run",
	}
	for sec, hint := range want {
		m := initialModel(&fakeLister{})
		m.section = sec
		got := m.hintLine()
		if !strings.Contains(got, hint) || !strings.HasSuffix(got, "? help - q quit") {
			t.Errorf("section %d footer = %q, want %q and the help and quit keys", sec, got, hint)
		}
	}
}

// TestHintLine_StateDependentKeys verifies the inspector and the ops output
// show their own keys rather than the pane's default ones.
func TestHintLine_StateDependentKeys(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section, m.mode = sectionFiles, modeInspect
	if got := m.hintLine(); !strings.Contains(got, "S scrub") {
		t.Errorf("inspector footer = %q, want the scrub key", got)
	}

	m = initialModel(&fakeLister{})
	m.section = sectionOps
	m.run.owner, m.run.shown, m.run.running = sectionOps, true, true
	if got := m.hintLine(); !strings.Contains(got, "up/down scroll") {
		t.Errorf("running output footer = %q, want scroll", got)
	}
	m.run.running = false
	if got := m.hintLine(); !strings.Contains(got, "esc close") {
		t.Errorf("finished output footer = %q, want esc close", got)
	}
}

// TestHelp_OpensAndClosesOnAnyKey verifies "?" replaces the pane with its
// keymap and the global keys, and the next key closes it without acting.
func TestHelp_OpensAndClosesOnAnyKey(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	m.handleKey(key("?"))
	if !m.help {
		t.Fatal("? did not open the help")
	}
	view := m.contentView()
	for _, want := range []string{"keys   Backends", "drain the backend", "everywhere", "Dashboard", "quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("help missing %q:\n%s", want, view)
		}
	}

	m.handleKey(key("d"))
	if m.help {
		t.Error("a key did not close the help")
	}
	if m.confirm != nil {
		t.Error("the key that closed the help also armed a drain")
	}
}

// TestGlobalKeys_NameEverySection verifies the help lists a jump for every
// nav destination.
func TestGlobalKeys_NameEverySection(t *testing.T) {
	t.Parallel()
	if len(sectionKeys) != selectableSections {
		t.Fatalf("%d section keys for %d sections", len(sectionKeys), selectableSections)
	}
	descs := map[string]bool{}
	for _, k := range globalKeys() {
		descs[k.desc] = true
	}
	for _, e := range navEntries() {
		if !descs[e.label] {
			t.Errorf("help has no jump to %s", e.label)
		}
	}
}

// TestReport_KeepsEveryResult verifies a result stays in the session log
// after the next keypress clears it from the footer.
func TestReport_KeepsEveryResult(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	m.applyBackendReconciled(backendReconciledMsg{backend: "minio-a", resp: &adminapi.ReconcileResponse{Imported: 2}})
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.status != nil {
		t.Error("the footer result survived a keypress")
	}
	if len(m.actionLog) != 1 || !m.actionLog[0].ok || !strings.Contains(m.actionLog[0].text, "imported 2") {
		t.Errorf("log = %+v, want the reconcile result", m.actionLog)
	}
}

// TestReport_KeepsTheNewest verifies the log drops its oldest results once it
// is full.
func TestReport_KeepsTheNewest(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	for i := range actionLogLimit + 5 {
		m.report(true, fmt.Sprintf("result %d", i))
	}
	if len(m.actionLog) != actionLogLimit {
		t.Fatalf("log holds %d, want %d", len(m.actionLog), actionLogLimit)
	}
	if got := m.actionLog[0].text; got != "result 5" {
		t.Errorf("oldest kept = %q, want result 5", got)
	}
}

// TestDashboard_ShowsRecentActions verifies the dashboard lists the session's
// results once there are any, newest last, and only as many as fit.
func TestDashboard_ShowsRecentActions(t *testing.T) {
	t.Parallel()
	m := dashboardModel(t)
	if strings.Contains(m.contentView(), "recent actions") {
		t.Error("an empty log should not take a pane")
	}
	for i := range 40 {
		m.report(i%2 == 0, fmt.Sprintf("action %02d", i))
	}
	view := m.contentView()
	if !strings.Contains(view, "recent actions") || !strings.Contains(view, "action 39") {
		t.Errorf("dashboard missing the newest action:\n%s", view)
	}
	if strings.Contains(view, "action 00") {
		t.Error("the dashboard showed more actions than fit")
	}
}

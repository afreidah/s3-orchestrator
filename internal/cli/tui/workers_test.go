// -------------------------------------------------------------------------------
// TUI - Workers View Tests
//
// Author: Alex Freidah
//
// Deterministic tests for the worker health pane: the health-to-row mapping,
// the never-ticked formatter, body rendering per state, key handling, and the
// split between a real fetch failure and a deployment that registers no
// workers at all.
// -------------------------------------------------------------------------------

package tui

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

func TestTickAge(t *testing.T) {
	t.Parallel()
	if got := tickAge(time.Time{}); got != "-" {
		t.Errorf("zero time = %q, want -", got)
	}
	if got := tickAge(time.Now().Add(-90 * time.Second)); got != "1m ago" {
		t.Errorf("90s ago = %q, want 1m ago", got)
	}
}

func TestRowsFromWorkers(t *testing.T) {
	t.Parallel()
	rows := rowsFromWorkers([]adminapi.WorkerHealth{
		{Name: "replicator", LastSuccess: time.Now().Add(-30 * time.Second)},
		{Name: "cleanup_queue", LastFailure: time.Now().Add(-2 * time.Minute), LastError: "connection refused", ConsecutiveFailures: 3},
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// A worker that has only ever succeeded shows a dash for its last failure.
	if r := rows[0]; r[0] != "replicator" || r[1] != "30s ago" || r[2] != "-" || r[3] != "0" || r[4] != "" {
		t.Errorf("replicator row = %v", r)
	}
	if r := rows[1]; r[1] != "-" || r[2] != "2m ago" || r[3] != "3" || r[4] != "connection refused" {
		t.Errorf("cleanup_queue row = %v", r)
	}
}

func TestFailingWorkers(t *testing.T) {
	t.Parallel()
	workers := []adminapi.WorkerHealth{
		{Name: "a"},
		{Name: "b", ConsecutiveFailures: 1},
		{Name: "c", ConsecutiveFailures: 9},
	}
	if got := failingWorkers(workers); got != 2 {
		t.Errorf("failingWorkers = %d, want 2", got)
	}
}

func TestApplyWorkers(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.workers = workersView{loading: true, unavailable: "stale", table: newTable()}
	m.resizeWorkers()
	m.applyWorkers(&adminapi.WorkersResponse{Workers: []adminapi.WorkerHealth{{Name: "scrubber"}}})

	if m.workers.loading || m.workers.unavailable != "" || m.workers.err != nil {
		t.Errorf("state = %+v", m.workers)
	}
	if len(m.workers.rows) != 1 || len(m.workers.table.Rows()) != 1 {
		t.Errorf("rows=%d tableRows=%d", len(m.workers.rows), len(m.workers.table.Rows()))
	}
}

// TestApplyWorkersErr_SeparatesUnavailable asserts the pane distinguishes a
// proxy-only deployment (503, a configuration fact) from a broken endpoint,
// because only the latter is worth showing as an error.
func TestApplyWorkersErr_SeparatesUnavailable(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})

	m.applyWorkersErr(&apiError{status: http.StatusServiceUnavailable, body: `{"error":"worker health not available"}`})
	if m.workers.err != nil || m.workers.unavailable != "worker health not available" {
		t.Errorf("503: err=%v unavailable=%q", m.workers.err, m.workers.unavailable)
	}

	m.applyWorkersErr(errors.New("boom"))
	if m.workers.err == nil || m.workers.unavailable != "" {
		t.Errorf("generic: err=%v unavailable=%q", m.workers.err, m.workers.unavailable)
	}
}

func TestWorkersBodyView_States(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})

	m.workers = workersView{err: errors.New("boom")}
	if got := m.workersBodyView(); !strings.Contains(got, "boom") {
		t.Errorf("error body = %q", got)
	}
	m.workers = workersView{unavailable: "worker health not available"}
	if got := m.workersBodyView(); !strings.Contains(got, "worker health not available") {
		t.Errorf("unavailable body = %q", got)
	}
	m.workers = workersView{loading: true}
	if got := m.workersBodyView(); !strings.Contains(got, "loading") {
		t.Errorf("loading body = %q", got)
	}
	m.workers = workersView{}
	if got := m.workersBodyView(); !strings.Contains(got, "no workers registered") {
		t.Errorf("empty body = %q", got)
	}
}

func TestWorkersHeaderView_CountAndFailing(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.workers.rows = []adminapi.WorkerHealth{{Name: "a"}, {Name: "b", ConsecutiveFailures: 2}}
	out := m.workersHeaderView()
	for _, want := range []string{"2 registered", "1 failing"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing %q: %q", want, out)
		}
	}

	// An all-healthy fleet says nothing about failures rather than "0 failing".
	m.workers.rows = []adminapi.WorkerHealth{{Name: "a"}}
	if out := m.workersHeaderView(); strings.Contains(out, "failing") {
		t.Errorf("healthy header mentions failing: %q", out)
	}
}

func TestHandleWorkersKey_BackAndReload(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionWorkers
	m.workers = workersView{table: newTable()}

	m.handleWorkersKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !m.navFocus || m.navCursor != int(sectionWorkers) {
		t.Errorf("after esc: navFocus=%v cursor=%d", m.navFocus, m.navCursor)
	}

	m.navFocus = false
	_, cmd := m.handleWorkersKey(key("r"))
	if !m.workers.loading || cmd == nil {
		t.Fatalf("reload: loading=%v cmd=%v", m.workers.loading, cmd)
	}
	if _, ok := cmd().(workersLoadedMsg); !ok {
		t.Errorf("reload result = %#v, want workersLoadedMsg", cmd())
	}
}

func TestLoadWorkers_Error(t *testing.T) {
	t.Parallel()
	cmd := initialModel(errLister{}).loadWorkers()
	if _, ok := cmd().(workersErrMsg); !ok {
		t.Errorf("cmd result = %#v, want workersErrMsg", cmd())
	}
}

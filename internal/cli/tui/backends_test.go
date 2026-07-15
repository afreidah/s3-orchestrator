// -------------------------------------------------------------------------------
// TUI - Backends View Tests
//
// Author: Alex Freidah
//
// Deterministic tests for the backends status pane: the status-to-row mapping,
// the health/drain formatters, body rendering per state, key handling (back,
// reload), and the load error path.
// -------------------------------------------------------------------------------

package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

func TestBackendHealthAndDrain(t *testing.T) {
	t.Parallel()
	if backendHealth(true) != "healthy" || backendHealth(false) != "unhealthy" {
		t.Error("backendHealth mapping wrong")
	}
	if backendDrain(true) != "draining" || backendDrain(false) != "-" {
		t.Error("backendDrain mapping wrong")
	}
}

func TestRowsFromBackends(t *testing.T) {
	t.Parallel()
	rows := rowsFromBackends([]adminapi.BackendStatus{
		{Name: "b1", Healthy: true, BytesUsed: 2048, BytesLimit: 4096, ObjectCount: 3, APIRequests: 9, IngressBytes: 1024, EgressBytes: 512},
		{Name: "b2", Healthy: false, Draining: true, BytesUsed: 0, BytesLimit: 0},
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// b1: healthy, human sizes, real limit.
	if r := rows[0]; r[0] != "b1" || r[1] != "healthy" || r[2] != "-" || r[3] != "2.0 KiB" || r[4] != "4.0 KiB" {
		t.Errorf("b1 row = %v", r)
	}
	// b2: unhealthy, draining, and a zero limit renders as a dash, not "0 B".
	if r := rows[1]; r[1] != "unhealthy" || r[2] != "draining" || r[4] != "-" {
		t.Errorf("b2 row = %v", r)
	}
}

func TestApplyStatus(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.backends = backendsView{loading: true, table: newTable()}
	m.resizeBackends()
	m.applyStatus(&adminapi.StatusResponse{
		DBHealthy:   true,
		UsagePeriod: "2026-07",
		Backends:    []adminapi.BackendStatus{{Name: "b1", Healthy: true}},
	})
	if m.backends.loading || !m.backends.dbHealthy || m.backends.usagePeriod != "2026-07" {
		t.Errorf("state = %+v", m.backends)
	}
	if len(m.backends.rows) != 1 || len(m.backends.table.Rows()) != 1 {
		t.Errorf("rows=%d tableRows=%d", len(m.backends.rows), len(m.backends.table.Rows()))
	}
}

func TestBackendsBodyView_States(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.backends.err = errors.New("boom")
	if got := m.backendsBodyView(); !strings.Contains(got, "boom") {
		t.Errorf("error body = %q", got)
	}
	m.backends = backendsView{loading: true}
	if got := m.backendsBodyView(); !strings.Contains(got, "loading") {
		t.Errorf("loading body = %q", got)
	}
	m.backends = backendsView{}
	if got := m.backendsBodyView(); !strings.Contains(got, "no backends") {
		t.Errorf("empty body = %q", got)
	}
}

func TestBackendsHeaderView_CountAndDBHealth(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.backends.rows = []adminapi.BackendStatus{{Name: "b1"}, {Name: "b2"}}
	m.backends.dbHealthy = true
	m.backends.usagePeriod = "2026-07"
	out := m.backendsHeaderView()
	for _, want := range []string{"2 configured", "db: healthy", "2026-07"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing %q: %q", want, out)
		}
	}
}

func TestHandleBackendsKey_BackAndReload(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionBackends
	m.backends = backendsView{table: newTable()}

	// esc returns focus to the sidebar, cursor on the current section.
	m.handleBackendsKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !m.navFocus || m.navCursor != int(sectionBackends) {
		t.Errorf("after esc: navFocus=%v cursor=%d", m.navFocus, m.navCursor)
	}

	// "r" reloads: sets loading and returns a fetch command.
	m.navFocus = false
	_, cmd := m.handleBackendsKey(key("r"))
	if !m.backends.loading || cmd == nil {
		t.Errorf("reload: loading=%v cmd=%v", m.backends.loading, cmd)
	}
	if _, ok := cmd().(statusLoadedMsg); !ok {
		t.Errorf("reload result = %#v, want statusLoadedMsg", cmd())
	}
}

func TestLoadStatus_Error(t *testing.T) {
	t.Parallel()
	cmd := initialModel(errLister{}).loadStatus()
	if _, ok := cmd().(statusErrMsg); !ok {
		t.Errorf("cmd result = %#v, want statusErrMsg", cmd())
	}
}

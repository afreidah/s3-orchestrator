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
	// b1: healthy, human sizes, real limit, use% = 2048/4096 = 50%.
	if r := rows[0]; r[0] != "b1" || r[1] != "healthy" || r[2] != "-" || r[3] != "2.0 KiB" || r[4] != "4.0 KiB" || r[5] != "50%" {
		t.Errorf("b1 row = %v", r)
	}
	// b2: unhealthy, draining, and a zero limit renders limit and use% as a dash.
	if r := rows[1]; r[1] != "unhealthy" || r[2] != "draining" || r[4] != "-" || r[5] != "-" {
		t.Errorf("b2 row = %v", r)
	}
}

func TestUsagePercent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		used, limit int64
		want        int
	}{
		{0, 4096, 0}, {2048, 4096, 50}, {4096, 4096, 100}, {5, 0, 0}, {5, -1, 0},
	}
	for _, c := range cases {
		if got := usagePercent(c.used, c.limit); got != c.want {
			t.Errorf("usagePercent(%d,%d) = %d, want %d", c.used, c.limit, got, c.want)
		}
	}
}

func TestUsageStyle_Thresholds(t *testing.T) {
	t.Parallel()
	// green under 70, yellow 70-89, red 90+.
	if usageStyle(20).GetForeground() != usageStyle(69).GetForeground() {
		t.Error("20 and 69 should share the low (green) style")
	}
	if usageStyle(75).GetForeground() == usageStyle(20).GetForeground() {
		t.Error("75 (warn) should differ from 20 (ok)")
	}
	if usageStyle(95).GetForeground() == usageStyle(75).GetForeground() {
		t.Error("95 (error) should differ from 75 (warn)")
	}
}

func TestBackendsStatsLine(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.backends.dbHealthy = true
	m.backends.rows = []adminapi.BackendStatus{
		{Name: "a", BytesUsed: 2048, BytesLimit: 4096},
		{Name: "b", BytesUsed: 1024, BytesLimit: 4096},
	}
	// total 3 KiB / 8 KiB = 37%, db healthy.
	got := m.backendsStatsLine()
	for _, want := range []string{"db:", "healthy", "total:", "37%"} {
		if !strings.Contains(got, want) {
			t.Errorf("stats line missing %q: %q", want, got)
		}
	}

	m.backends.dbHealthy = false
	if got := m.backendsStatsLine(); !strings.Contains(got, "UNAVAILABLE") {
		t.Errorf("unhealthy stats line = %q", got)
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

// TestIntegrityCoverage renders each state the verification summary can be in.
// The never-verified case is the one that matters: it is what a fleet whose
// scrubber has never reached it looks like.
func TestIntegrityCoverage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   adminapi.IntegrityStatus
		want string
	}{
		{"never verified dominates", adminapi.IntegrityStatus{NeverVerifiedCopies: 132986, OldestUnverifiedSeconds: 90}, "132,986 never"},
		{"fully swept", adminapi.IntegrityStatus{}, "up to date"},
		{"oldest age once everything has been seen", adminapi.IntegrityStatus{OldestUnverifiedSeconds: 7200}, "2h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := initialModel(&fakeLister{})
			m.backends.integrity = tt.in
			if got := m.integrityCoverage(); !strings.Contains(got, tt.want) {
				t.Errorf("integrityCoverage() = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

// TestBackendsStatsLine_CarriesIntegrity verifies the verification summary
// reaches the stats line, not just its own helper.
func TestBackendsStatsLine_CarriesIntegrity(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.backends.dbHealthy = true
	m.backends.integrity = adminapi.IntegrityStatus{NeverVerifiedCopies: 7}
	if got := m.backendsStatsLine(); !strings.Contains(got, "verified:") {
		t.Errorf("stats line missing the verification summary: %q", got)
	}
}

// TestEncryptionCoverage pins when the plaintext count appears at all. It is a
// standing gap that only an operator action closes, so it stays visible while
// non-zero and disappears entirely once the fleet is fully encrypted rather
// than sitting on the stats line reading zero.
func TestEncryptionCoverage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		plaintext int64
		want      string
	}{
		{"fully encrypted renders nothing", 0, ""},
		{"negative renders nothing", -1, ""},
		{"outstanding copies are shown", 1234, "plaintext:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := initialModel(&fakeLister{})
			m.backends.integrity = adminapi.IntegrityStatus{PlaintextCopies: tt.plaintext}

			got := m.encryptionCoverage()
			if tt.want == "" {
				if got != "" {
					t.Errorf("encryptionCoverage() = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("encryptionCoverage() = %q, want it to contain %q", got, tt.want)
			}
			if !strings.Contains(got, "1,234") {
				t.Errorf("encryptionCoverage() = %q, want a thousands-separated count", got)
			}
		})
	}
}

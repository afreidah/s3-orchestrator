// -------------------------------------------------------------------------------
// TUI - Dashboard Tests
//
// Author: Alex Freidah
//
// Covers that the TUI opens on the dashboard, what its capacity and summary
// blocks say for a loaded fleet, how figures that have not arrived yet read,
// and that the pane fits the terminal at a narrow width.
// -------------------------------------------------------------------------------

package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// dashboardModel returns a sized model on the dashboard with a status,
// replication and cleanup snapshot applied: one backend near full and
// draining, one unhealthy with no limit.
func dashboardModel(t *testing.T) *model {
	t.Helper()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 30
	m.applyStatus(&adminapi.StatusResponse{
		DBHealthy:   true,
		UsagePeriod: "2026-10",
		Backends: []adminapi.BackendStatus{
			{Name: "minio-a", Healthy: true, DrainState: "draining", BytesUsed: 9 << 30, BytesLimit: 10 << 30,
				APIRequests: 1200, IngressBytes: 1 << 30, CompressionSavedBytes: 2 << 30},
			{Name: "minio-b", Healthy: false, BytesUsed: 1 << 30, APIRequests: 34, EgressBytes: 3 << 30},
		},
		Integrity: adminapi.IntegrityStatus{OldestUnverifiedSeconds: 3600, DeferredCopies: 3, PlaintextCopies: 1500},
	})
	m.applyReplication(&adminapi.ReplicationStatusResponse{Factor: 2, UnderReplicated: 12, ComputedAt: time.Now()})
	m.applyCleanup(cleanupLoadedMsg{
		queue: &adminapi.CleanupQueueResponse{Depth: 4},
		dlq:   &adminapi.CleanupDLQResponse{Depth: 2},
	})
	return m
}

// assertContains fails the test for every want missing from got.
func assertContains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestDashboard_IsWhereTheTUIOpens verifies a new model starts on the
// dashboard and renders it.
func TestDashboard_IsWhereTheTUIOpens(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	if m.section != sectionDashboard {
		t.Fatalf("section = %d, want the dashboard", m.section)
	}
	m.width, m.height = 120, 30
	assertContains(t, m.contentView(), "dashboard", "loading")
}

// TestDashboard_Capacity verifies each backend gets a bar with its figures and
// markers, a backend with no limit says so, and the fleet total counts only
// the limits that exist.
func TestDashboard_Capacity(t *testing.T) {
	t.Parallel()
	got := dashboardModel(t).dashboardCapacity()
	assertContains(t, got,
		"minio-a", "9.0 GiB / 10.0 GiB", "90%", "draining",
		"minio-b", "no limit", "UNHEALTHY",
		"fleet", "10.0 GiB / 10.0 GiB", "100%",
		"[", "#", "-]")
}

// TestDashboard_Summary verifies the summary block reports every fleet-wide
// figure from the three snapshots.
func TestDashboard_Summary(t *testing.T) {
	t.Parallel()
	got := dashboardModel(t).dashboardSummary()
	assertContains(t, got,
		"database", "healthy",
		"factor 2", "12 under", "0 over",
		"oldest 1h", "3 unreachable",
		"1,500 plaintext copies",
		"2.0 GiB saved",
		"1,234 requests", "1.0 GiB in", "3.0 GiB out", "period 2026-10",
		"4 pending", "2 dead-lettered")
}

// TestDashboard_FiguresNotYetLoaded verifies replication and cleanup read as a
// dash until their snapshots arrive, rather than as zero.
func TestDashboard_FiguresNotYetLoaded(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.applyStatus(&adminapi.StatusResponse{})
	if got := m.dashboardReplication(); !strings.Contains(got, "-") {
		t.Errorf("replication before its snapshot = %q, want a dash", got)
	}
	if got := m.dashboardCleanup(); !strings.Contains(got, "-") {
		t.Errorf("cleanup before its snapshot = %q, want a dash", got)
	}
}

// TestDashboard_QuietFleet verifies the summary for a fleet with nothing to
// report: replication off, every copy encrypted, nothing compressed, an empty
// dead-letter table, and the database down.
func TestDashboard_QuietFleet(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.applyStatus(&adminapi.StatusResponse{DBHealthy: false})
	m.applyReplication(&adminapi.ReplicationStatusResponse{Factor: 1})
	m.applyCleanup(cleanupLoadedMsg{queue: &adminapi.CleanupQueueResponse{}, dlq: &adminapi.CleanupDLQResponse{}})

	assertContains(t, m.dashboardSummary(),
		"UNAVAILABLE", "disabled", "every copy encrypted", "nothing stored compressed",
		"0 pending", "0 dead-lettered")
}

// TestDashboard_StatusError verifies a failed status fetch takes the body.
func TestDashboard_StatusError(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.backends.err = errors.New("boom")
	assertContains(t, bodyText(m.dashboardBody()), "boom")
}

// TestUsageBar_StaysWithinItsWidth verifies a backend past its limit fills
// the bar without overrunning it.
func TestUsageBar_StaysWithinItsWidth(t *testing.T) {
	t.Parallel()
	for _, pct := range []int{0, 50, 100, 140} {
		if w := lipgloss.Width(usageBar(pct, 20)); w != 22 {
			t.Errorf("usageBar(%d) is %d wide, want 22", pct, w)
		}
	}
}

// TestDashboard_FitsANarrowTerminal verifies the dashboard renders to exactly
// the terminal's height without running past the content area.
func TestDashboard_FitsANarrowTerminal(t *testing.T) {
	t.Parallel()
	m := dashboardModel(t)
	m.width, m.height = 80, 20
	view := m.contentView()
	if got := lipgloss.Height(view); got != m.height {
		t.Errorf("rendered %d lines, want %d", got, m.height)
	}
	for _, line := range strings.Split(view, "\n") {
		if w := lipgloss.Width(line); w > m.contentWidth() {
			t.Errorf("line is %d wide, past the %d-column content area: %q", w, m.contentWidth(), line)
		}
	}
}

// TestHandleDashboardKey_BackAndReload verifies esc hands focus to the nav and
// "r" requests the dashboard's snapshots.
func TestHandleDashboardKey_BackAndReload(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})

	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !m.navFocus || m.navCursor != int(sectionDashboard) {
		t.Errorf("after esc: navFocus=%v cursor=%d", m.navFocus, m.navCursor)
	}

	m.navFocus = false
	if _, cmd := m.handleKey(key("r")); cmd == nil {
		t.Fatal("reload should request the snapshots")
	}
	for _, target := range []pollTarget{pollStatus, pollReplication, pollCleanup} {
		if !m.poll.inFlight[target] {
			t.Errorf("reload did not request target %d", target)
		}
	}
}

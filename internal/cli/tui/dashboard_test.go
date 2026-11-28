// -------------------------------------------------------------------------------
// TUI - Dashboard Tests
//
// Author: Alex Freidah
//
// Covers that the TUI opens on the dashboard, what its summary and backend
// blocks say for a loaded fleet, how figures that have not arrived yet read,
// that the blocks scroll beneath a summary that stays put, and that the pane
// fits the terminal at a narrow width.
// -------------------------------------------------------------------------------

package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// dashboardModel returns a sized model on the dashboard with a status,
// replication and cleanup snapshot applied: one backend near full, draining,
// at its egress limit and part way through a request budget, one refusing GET
// with no limits, and one refusing every call that reports no usage.
func dashboardModel(t *testing.T) *model {
	t.Helper()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 30
	m.applyStatus(&adminapi.StatusResponse{
		DBHealthy: true,
		Backends: []adminapi.BackendStatus{
			{Name: "minio-a", Healthy: true, Flags: []string{"draining"}, BytesUsed: 9 << 30, BytesLimit: 10 << 30,
				APIRequests: 1200, IngressBytes: 1 << 30, CompressionSavedBytes: 2 << 30,
				UsageLimits: []adminapi.UsageMeter{
					{Kind: "egress", Used: 5 << 30, Limit: 5 << 30},
					{Kind: "ingress", Used: 1 << 30},
					{Kind: "requests", Budget: "writes", Used: 1200, Limit: 4800},
				},
				UsageCycle: wireCycle(15, "America/Los_Angeles", -7)},
			{Name: "minio-b", RefusedCalls: []string{"GetObject"}, BytesUsed: 1 << 30, APIRequests: 34, EgressBytes: 3 << 30,
				UsageLimits: []adminapi.UsageMeter{
					{Kind: "egress", Used: 3 << 30},
					{Kind: "ingress"},
					{Kind: "requests", Used: 34},
				},
				UsageCycle: wireCycle(1, "UTC", 0)},
			{Name: "minio-c", RefusedCalls: []string{"PutObject", "GetObject", "HeadObject", "DeleteObject", "DeleteObjects", "CopyObject", "ListObjects"}},
		},
		Integrity: adminapi.IntegrityStatus{OldestUnverifiedSeconds: 3600, DeferredCopies: 3, PlaintextCopies: 1500},
	})
	m.applyReplication(&adminapi.ReplicationStatusResponse{Factor: 2, UnderReplicated: 12, ComputedAt: time.Now()})
	m.applyCleanup(cleanupListings{
		queue: &adminapi.CleanupQueueResponse{Depth: 4},
		dlq:   &adminapi.CleanupDLQResponse{Depth: 2},
	})
	return m
}

// wireCycle builds an October-to-November usage cycle starting on day in a
// zone with the given UTC offset, the way it arrives in a status response.
func wireCycle(day int, zone string, offsetHours int) adminapi.UsageCycle {
	loc := time.FixedZone(zone, offsetHours*3600)
	start := time.Date(2026, 10, day, 0, 0, 0, 0, loc)
	return adminapi.UsageCycle{Start: start, NextReset: start.AddDate(0, 1, 0), Timezone: zone}
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

// dashboardBlocks renders m's backend blocks and returns each one, keyed by
// the backend its rule names.
func dashboardBlocks(t *testing.T, m *model) map[string]string {
	t.Helper()
	blocks := map[string]string{}
	for block := range strings.SplitSeq(m.dashboardBackends(m.dashboardColumns()), "\n\n") {
		rule, _, _ := strings.Cut(block, "\n")
		name := strings.Fields(strings.TrimLeft(rule, ruleRune+" "))[0]
		blocks[name] = block
	}
	return blocks
}

// TestDashboard_BackendBlocks verifies every backend gets a block opened by a
// rule naming it with its health, flags and usage cycle, over its storage and
// a row per usage dimension: bytes for egress and ingress, a call count for
// requests, no limit where the backend sets none, and a limit used up drawn as
// a full bar. A backend reporting no usage shows its storage alone and no
// cycle.
func TestDashboard_BackendBlocks(t *testing.T) {
	t.Parallel()
	blocks := dashboardBlocks(t, dashboardModel(t))
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want one per backend: %v", len(blocks), blocks)
	}
	a := blocks["minio-a"]
	rule, rows, _ := strings.Cut(a, "\n")
	assertContains(t, rule, "minio-a", "healthy", "draining", "Oct 15 - Nov 15 America/Los_Angeles", ruleRune)
	assertContains(t, rows,
		"storage", "9.0 GiB / 10.0 GiB", "90%",
		"egress", "5.0 GiB / 5.0 GiB", "100%",
		"ingress", "no limit", "1.0 GiB",
		"requests:writes", "1,200 / 4,800", "25%")
	if strings.Contains(rows, "Oct 15") {
		t.Errorf("minio-a's rows repeat its cycle, which its rule carries:\n%s", rows)
	}
	for line := range strings.SplitSeq(rows, "\n") {
		if strings.Contains(line, "egress") && strings.Contains(line, "-]") {
			t.Errorf("the exhausted limit is not drawn as a full bar: %q", line)
		}
	}
	assertContains(t, blocks["minio-b"], "DEGRADED GetObject", "Oct 1 - Nov 1 UTC", "storage", "no limit", "3.0 GiB", "34")
	c := blocks["minio-c"]
	assertContains(t, c, "DOWN", "storage")
	if strings.Contains(c, "Oct") || strings.Count(c, "\n") != 1 {
		t.Errorf("minio-c reports no usage, want its rule and storage alone:\n%s", c)
	}
}

// TestDashboard_MetersAlign verifies the summary's fleet storage and every
// backend block share their columns, so every bar starts in one column.
func TestDashboard_MetersAlign(t *testing.T) {
	t.Parallel()
	m := dashboardModel(t)
	cols := m.dashboardColumns()
	starts := map[int]bool{}
	for _, block := range []string{m.dashboardSummary(cols), m.dashboardBackends(cols)} {
		for line := range strings.SplitSeq(block, "\n") {
			if i := strings.Index(line, "["); i >= 0 {
				starts[i] = true
			}
		}
	}
	if len(starts) != 1 {
		t.Errorf("bars start in %d different columns, want 1: %v", len(starts), starts)
	}
}

// TestDashboard_BlocksCarryTheirOwnCycle verifies each backend's rule carries
// that backend's own cycle, not one fleet-wide period.
func TestDashboard_BlocksCarryTheirOwnCycle(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 140, 30
	egress := []adminapi.UsageMeter{{Kind: "egress", Used: 1, Limit: 10}}
	m.applyStatus(&adminapi.StatusResponse{Backends: []adminapi.BackendStatus{
		{Name: "c2", UsageLimits: egress, UsageCycle: wireCycle(15, "UTC", 0)},
		{Name: "r2", UsageLimits: egress, UsageCycle: wireCycle(1, "America/Los_Angeles", -7)},
	}})
	blocks := dashboardBlocks(t, m)
	for name, cycle := range map[string]string{"c2": "Oct 15 - Nov 15 UTC", "r2": "Oct 1 - Nov 1 America/Los_Angeles"} {
		if rule, _, _ := strings.Cut(blocks[name], "\n"); !strings.Contains(rule, cycle) {
			t.Errorf("%s rule lacks its cycle %q: %q", name, cycle, rule)
		}
	}
}

// TestDashboard_RuleDropsTheCycleWhenCramped verifies a rule too narrow for a
// backend's name and cycle together keeps the name and fits the width.
func TestDashboard_RuleDropsTheCycleWhenCramped(t *testing.T) {
	t.Parallel()
	got := ruleLine("minio-a  healthy", "Oct 15 - Nov 15 America/Los_Angeles", 30)
	if !strings.Contains(got, "minio-a") || strings.Contains(got, "Oct") || lipgloss.Width(got) != 30 {
		t.Errorf("ruleLine = %q (%d wide), want the name alone across 30 cells", got, lipgloss.Width(got))
	}
}

// TestDashboard_ScrollsTheBackends verifies on a terminal too short for every
// block that the summary stays at the top while the down key scrolls the
// blocks, until the last backend comes into view.
func TestDashboard_ScrollsTheBackends(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 24
	backends := make([]adminapi.BackendStatus, 12)
	for i := range backends {
		backends[i] = adminapi.BackendStatus{Name: fmt.Sprintf("backend-%02d", i), BytesUsed: 1,
			UsageLimits: []adminapi.UsageMeter{{Kind: "egress", Used: 1, Limit: 10}}}
	}
	m.applyStatus(&adminapi.StatusResponse{DBHealthy: true, Backends: backends})

	first, last := ruleRune+ruleRune+" backend-00", ruleRune+ruleRune+" backend-11"
	view := m.contentView()
	assertContains(t, view, "fleet storage", first)
	if strings.Contains(view, last) {
		t.Fatalf("every block fit a 24-row terminal, so nothing scrolls:\n%s", view)
	}
	for range 60 {
		m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
		_ = m.contentView()
	}
	view = m.contentView()
	assertContains(t, view, "fleet storage", last)
	if strings.Contains(view, first) {
		t.Errorf("scrolled to the end and the first block still shows:\n%s", view)
	}
	if got := lipgloss.Height(view); got != m.height {
		t.Errorf("rendered %d lines, want %d", got, m.height)
	}
}

// TestDashboard_SummaryAboveTheBlocks verifies the summary is its own pane
// above the scrolling blocks, so scrolling never moves it.
func TestDashboard_SummaryAboveTheBlocks(t *testing.T) {
	t.Parallel()
	body := dashboardModel(t).dashboardBody()
	if first := body[0].render(body[0].height); !strings.Contains(first, "nearest limit") {
		t.Errorf("first pane = %q, want the summary", first)
	}
	if last := body[len(body)-1]; !last.grows {
		t.Error("the backend blocks are not the growing pane at the bottom")
	}
}

// TestDashboard_Summary verifies the summary block reports every fleet-wide
// figure from the three snapshots, the fleet's storage counting only the
// limits that exist.
func TestDashboard_Summary(t *testing.T) {
	t.Parallel()
	m := dashboardModel(t)
	got := m.dashboardSummary(m.dashboardColumns())
	assertContains(t, got,
		"fleet storage", "10.0 GiB / 10.0 GiB",
		"nearest limit", "minio-a egress", "100%", "5.0 GiB / 5.0 GiB", "resets in",
		"database", "healthy",
		"factor 2", "12 under", "0 over",
		"oldest 1h", "3 unreachable",
		"1,500 plaintext copies",
		"2.0 GiB saved",
		"1,234 requests", "1.0 GiB in", "3.0 GiB out", "current cycles",
		"4 pending", "2 dead-lettered")
}

// TestDashboard_NearestLimit verifies the tile names the fullest limit across
// storage quotas and usage limits, how long until a usage limit resets, no
// reset for a storage quota, and says so when no backend has a limit.
func TestDashboard_NearestLimit(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	next := time.Now().Add(4*24*time.Hour + time.Hour)
	m.applyStatus(&adminapi.StatusResponse{Backends: []adminapi.BackendStatus{
		{Name: "c2", BytesUsed: 1, BytesLimit: 10,
			UsageLimits: []adminapi.UsageMeter{{Kind: "egress", Used: 87, Limit: 100}},
			UsageCycle:  adminapi.UsageCycle{NextReset: next}},
		{Name: "r2", BytesUsed: 5, BytesLimit: 10},
	}})
	assertContains(t, m.dashboardNearestLimit(), "c2 egress", "87%", "87 B / 100 B", "resets in 4d")

	m.backends.list.received[1].BytesUsed = 9
	got := m.dashboardNearestLimit()
	assertContains(t, got, "r2 storage", "90%")
	if strings.Contains(got, "resets") {
		t.Errorf("a storage quota shows a reset: %q", got)
	}

	m.applyStatus(&adminapi.StatusResponse{Backends: []adminapi.BackendStatus{{Name: "b1", BytesUsed: 1}}})
	assertContains(t, m.dashboardNearestLimit(), "no limits set")
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
	m.applyCleanup(cleanupListings{queue: &adminapi.CleanupQueueResponse{}, dlq: &adminapi.CleanupDLQResponse{}})

	assertContains(t, m.dashboardSummary(m.dashboardColumns()),
		"UNAVAILABLE", "disabled", "every copy encrypted", "nothing stored compressed",
		"0 pending", "0 dead-lettered")
}

// TestDashboard_ReplicationNamesBucketFactors verifies a deployment at factor
// 1 whose virtual buckets set their own reads as replicating, with how many
// set their own beside the backlog.
func TestDashboard_ReplicationNamesBucketFactors(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	snap := &adminapi.ReplicationStatusResponse{Replicating: true, UnderReplicated: 4}
	snap.Factor, snap.BucketFactors = 1, map[string]int{"photos": 2}
	m.applyReplication(snap)
	got := m.dashboardReplication()
	assertContains(t, got, "factor 1", "1 bucket sets its own", "4 under", "0 over")
	if strings.Contains(got, "disabled") {
		t.Errorf("replication = %q, want it running while a bucket sets a factor", got)
	}

	snap.BucketFactors["wal"] = 1
	assertContains(t, m.dashboardReplication(), "2 buckets set their own")
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
	for line := range strings.SplitSeq(view, "\n") {
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

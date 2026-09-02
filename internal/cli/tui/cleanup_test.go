// -------------------------------------------------------------------------------
// TUI - Cleanup View Tests
//
// Author: Alex Freidah
//
// Deterministic tests for the cleanup pane: both row mappings, the tab toggle,
// header depths, body rendering per state and per tab, and the requeue action -
// including that it is refused on the pending listing, which it does not apply
// to, and that a successful requeue reloads so the depths stay honest.
// -------------------------------------------------------------------------------

package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// cleanupModel builds a sized model on the cleanup section with both listings
// loaded, so tests exercise the pane in the state a user actually sees.
func cleanupModel(t *testing.T) *model {
	t.Helper()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.section = sectionCleanup
	m.cleanup = cleanupView{queue: newTable(), dlq: newTable()}
	m.resizeCleanup()
	m.applyCleanup(cleanupLoadedMsg{
		queue: &adminapi.CleanupQueueResponse{
			Depth: 7,
			Items: []adminapi.CleanupQueueItem{
				{ID: 1, Backend: "b1", ObjectKey: "a/1", SizeBytes: 1024, Attempts: 2, ClaimedBy: "inst-1"},
			},
		},
		dlq: &adminapi.CleanupDLQResponse{
			Depth: 3,
			Items: []adminapi.CleanupDLQItem{
				{Backend: "b2", ObjectKey: "b/2", SizeBytes: 2048, Attempts: 10, MovedAt: time.Now().Add(-time.Hour)},
			},
		},
	})
	return m
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

func TestRowsFromCleanupQueue(t *testing.T) {
	t.Parallel()
	rows := rowsFromCleanupQueue([]adminapi.CleanupQueueItem{
		{Backend: "b1", ObjectKey: "a/1", SizeBytes: 1024, Attempts: 2, ClaimedBy: "inst-1"},
		{Backend: "b2", ObjectKey: "a/2", SizeBytes: 0, Attempts: 0},
	})
	if r := rows[0]; r[0] != "a/1" || r[1] != "b1" || r[2] != "1.0 KiB" || r[3] != "2" || r[4] != "inst-1" {
		t.Errorf("claimed row = %v", r)
	}
	// An unclaimed row shows a dash rather than an empty cell.
	if r := rows[1]; r[4] != "-" {
		t.Errorf("unclaimed row = %v", r)
	}
}

func TestRowsFromCleanupDLQ(t *testing.T) {
	t.Parallel()
	rows := rowsFromCleanupDLQ([]adminapi.CleanupDLQItem{
		{Backend: "b2", ObjectKey: "b/2", SizeBytes: 2048, Attempts: 10, MovedAt: time.Now().Add(-2 * time.Hour)},
	})
	if r := rows[0]; r[0] != "b/2" || r[1] != "b2" || r[2] != "2.0 KiB" || r[3] != "10" || r[4] != "2h ago" {
		t.Errorf("dlq row = %v", r)
	}
}

func TestApplyCleanup(t *testing.T) {
	t.Parallel()
	m := cleanupModel(t)
	if m.cleanup.loading || m.cleanup.err != nil {
		t.Errorf("state = %+v", m.cleanup)
	}
	if m.cleanup.queueDepth != 7 || m.cleanup.dlqDepth != 3 {
		t.Errorf("depths = %d / %d, want 7 / 3", m.cleanup.queueDepth, m.cleanup.dlqDepth)
	}
	if len(m.cleanup.queue.Rows()) != 1 || len(m.cleanup.dlq.Rows()) != 1 {
		t.Errorf("table rows = %d / %d", len(m.cleanup.queue.Rows()), len(m.cleanup.dlq.Rows()))
	}
}

// TestCleanupHeaderView_ShowsBothDepths asserts the depth of the listing that
// is not on screen stays visible, so a backlog in the other tab is not hidden.
func TestCleanupHeaderView_ShowsBothDepths(t *testing.T) {
	t.Parallel()
	m := cleanupModel(t)
	out := m.cleanupHeaderView()
	for _, want := range []string{"pending 7", "dead-letter 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing %q: %q", want, out)
		}
	}
	// The active tab is bracketed.
	if !strings.Contains(out, "[pending 7]") {
		t.Errorf("queue tab not marked active: %q", out)
	}
	m.cleanup.tab = cleanupTabDLQ
	if out := m.cleanupHeaderView(); !strings.Contains(out, "[dead-letter 3]") {
		t.Errorf("dlq tab not marked active: %q", out)
	}
}

func TestHandleCleanupKey_TogglesTab(t *testing.T) {
	t.Parallel()
	m := cleanupModel(t)
	if m.cleanupOnDLQ() {
		t.Fatal("pane should open on the pending listing")
	}
	m.handleCleanupKey(key("t"))
	if !m.cleanupOnDLQ() {
		t.Error("t did not switch to the dead-letter listing")
	}
	m.handleCleanupKey(key("t"))
	if m.cleanupOnDLQ() {
		t.Error("t did not switch back to the pending listing")
	}
}

func TestHandleCleanupKey_BackAndReload(t *testing.T) {
	t.Parallel()
	m := cleanupModel(t)

	m.handleCleanupKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !m.navFocus || m.navCursor != int(sectionCleanup) {
		t.Errorf("after esc: navFocus=%v cursor=%d", m.navFocus, m.navCursor)
	}

	m.navFocus = false
	_, cmd := m.handleCleanupKey(key("r"))
	if !m.cleanup.loading || cmd == nil {
		t.Fatalf("reload: loading=%v cmd=%v", m.cleanup.loading, cmd)
	}
	if _, ok := cmd().(cleanupLoadedMsg); !ok {
		t.Errorf("reload result = %#v, want cleanupLoadedMsg", cmd())
	}
}

// TestArmRequeue_ConfirmsSelectedBackend asserts requeue arms a confirmation
// naming the backend, not the highlighted key: the endpoint moves every
// dead-lettered row for that backend, so a key-shaped prompt would misstate
// the blast radius.
func TestArmRequeue_ConfirmsSelectedBackend(t *testing.T) {
	t.Parallel()
	m := cleanupModel(t)
	m.cleanup.tab = cleanupTabDLQ

	m.handleCleanupKey(key("R"))
	if m.confirm == nil {
		t.Fatal("requeue did not arm a confirmation")
	}
	if !strings.Contains(m.confirm.text, "b2") {
		t.Errorf("confirm text = %q, want the backend name", m.confirm.text)
	}
	if !strings.Contains(m.confirm.text, "every") {
		t.Errorf("confirm text = %q, want it to state the whole-backend scope", m.confirm.text)
	}
}

// TestArmRequeue_RefusedOnPendingTab covers pressing R on the listing requeue
// does not apply to: it must explain itself rather than silently do nothing or
// act on a row from the other tab.
func TestArmRequeue_RefusedOnPendingTab(t *testing.T) {
	t.Parallel()
	m := cleanupModel(t)
	m.handleCleanupKey(key("R"))
	if m.confirm != nil {
		t.Fatal("requeue armed on the pending listing")
	}
	if m.status == nil || !strings.Contains(m.status.text, "dead-letter") {
		t.Errorf("status = %+v, want an explanation", m.status)
	}
}

// TestArmRequeue_EmptyDLQ covers pressing R with nothing to select.
func TestArmRequeue_EmptyDLQ(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.section = sectionCleanup
	m.cleanup = cleanupView{tab: cleanupTabDLQ, queue: newTable(), dlq: newTable()}
	m.resizeCleanup()

	m.handleCleanupKey(key("R"))
	if m.confirm != nil {
		t.Error("requeue armed with no dead-lettered rows")
	}
}

func TestApplyCleanupRequeued(t *testing.T) {
	t.Parallel()
	m := cleanupModel(t)

	_, cmd := m.applyCleanupRequeued(cleanupRequeuedMsg{
		resp: &adminapi.CleanupDLQRequeueResponse{Backend: "b2", Requeued: 4},
	})
	if m.status == nil || !m.status.ok {
		t.Fatalf("status = %+v, want an ok result", m.status)
	}
	for _, want := range []string{"4 rows", "b2"} {
		if !strings.Contains(m.status.text, want) {
			t.Errorf("status %q missing %q", m.status.text, want)
		}
	}
	// The depths just changed, so the pane must refetch rather than show stale ones.
	if !m.cleanup.loading || cmd == nil {
		t.Errorf("requeue did not trigger a reload: loading=%v cmd=%v", m.cleanup.loading, cmd)
	}
}

func TestApplyCleanupRequeued_AllBackends(t *testing.T) {
	t.Parallel()
	m := cleanupModel(t)
	m.applyCleanupRequeued(cleanupRequeuedMsg{resp: &adminapi.CleanupDLQRequeueResponse{Requeued: 1}})
	if m.status == nil || !strings.Contains(m.status.text, "all backends") {
		t.Errorf("status = %+v, want the unscoped wording", m.status)
	}
}

func TestApplyCleanupRequeued_Error(t *testing.T) {
	t.Parallel()
	m := cleanupModel(t)
	m.applyCleanupRequeued(cleanupRequeuedMsg{err: errors.New("boom")})
	if m.status == nil || m.status.ok || !strings.Contains(m.status.text, "boom") {
		t.Errorf("status = %+v, want a failure", m.status)
	}
	if m.cleanup.loading {
		t.Error("a failed requeue should not trigger a reload")
	}
}

func TestCleanupBodyView_States(t *testing.T) {
	t.Parallel()
	m := cleanupModel(t)

	m.cleanup.err = errors.New("boom")
	if got := m.cleanupBodyView(); !strings.Contains(got, "boom") {
		t.Errorf("error body = %q", got)
	}
	m.cleanup.err = nil
	m.cleanup.loading = true
	if got := m.cleanupBodyView(); !strings.Contains(got, "loading") {
		t.Errorf("loading body = %q", got)
	}

	// An empty queue is good news, so it reads as a state rather than an absence.
	m.cleanup = cleanupView{queue: newTable(), dlq: newTable()}
	if got := m.cleanupBodyView(); !strings.Contains(got, "cleanup queue is empty") {
		t.Errorf("empty queue body = %q", got)
	}
	m.cleanup.tab = cleanupTabDLQ
	if got := m.cleanupBodyView(); !strings.Contains(got, "no dead-lettered cleanups") {
		t.Errorf("empty dlq body = %q", got)
	}
}

// TestCleanupFooterView_OffersRequeueOnlyOnDLQ asserts the hints do not
// advertise an action the active listing cannot perform.
func TestCleanupFooterView_OffersRequeueOnlyOnDLQ(t *testing.T) {
	t.Parallel()
	m := cleanupModel(t)
	if got := m.cleanupFooterView(); strings.Contains(got, "requeue") {
		t.Errorf("pending footer offers requeue: %q", got)
	}
	m.cleanup.tab = cleanupTabDLQ
	if got := m.cleanupFooterView(); !strings.Contains(got, "requeue") {
		t.Errorf("dlq footer = %q, want a requeue hint", got)
	}
}

func TestLoadCleanup_Error(t *testing.T) {
	t.Parallel()
	cmd := initialModel(errLister{}).loadCleanup()
	if _, ok := cmd().(cleanupErrMsg); !ok {
		t.Errorf("cmd result = %#v, want cleanupErrMsg", cmd())
	}
}

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
		{"unreachable copies are named", adminapi.IntegrityStatus{DeferredCopies: 2686}, "2,686 unreachable"},
		{"a clean reachable sweep still shows what it could not reach",
			adminapi.IntegrityStatus{DeferredCopies: 5}, "up to date"},
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

// -------------------------------------------------------------------------
// PER-BACKEND ACTIONS
// -------------------------------------------------------------------------

// backendsModel returns a model on the backends pane with two rows loaded and
// the cursor on the first.
func backendsModel(t *testing.T, f *fakeLister) *model {
	t.Helper()
	m := initialModel(f)
	m.section = sectionBackends
	m.width, m.height = 120, 30
	m.resizeBackends()
	m.applyStatus(&adminapi.StatusResponse{Backends: []adminapi.BackendStatus{
		{Name: "minio-a", Healthy: true},
		{Name: "minio-b", Healthy: true},
	}})
	return m
}

// accept resolves an armed confirmation and runs the command it holds,
// returning the message that command produced.
func accept(t *testing.T, m *model) tea.Msg {
	t.Helper()
	if m.confirm == nil {
		t.Fatal("no confirmation was armed")
	}
	_, cmd := m.handleConfirmKey(key("y"))
	if cmd == nil {
		t.Fatal("accepting the confirmation produced no command")
	}
	return cmd()
}

// TestBackendActions_ConfirmNamesTheRow asserts each action confirms against
// the highlighted backend, so a keystroke on the wrong row is visible before
// it acts.
func TestBackendActions_ConfirmNamesTheRow(t *testing.T) {
	t.Parallel()
	cases := []struct{ key, want string }{
		{"d", "minio-a"},
		{"R", "minio-a"},
		{"Q", "minio-a"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			m := backendsModel(t, &fakeLister{})
			m.handleBackendsKey(key(tc.key))
			if m.confirm == nil {
				t.Fatalf("%q armed no confirmation", tc.key)
			}
			if !strings.Contains(m.confirm.text, tc.want) {
				t.Errorf("confirm = %q, want it to name %q", m.confirm.text, tc.want)
			}
		})
	}
}

// TestBackendActions_ActOnTheSecondRow asserts the action follows the cursor
// rather than always hitting the first backend.
func TestBackendActions_ActOnTheSecondRow(t *testing.T) {
	t.Parallel()
	f := &fakeLister{}
	m := backendsModel(t, f)
	m.backends.table.SetCursor(1)

	m.handleBackendsKey(key("R"))
	if !strings.Contains(m.confirm.text, "minio-b") {
		t.Fatalf("confirm = %q, want it to name minio-b", m.confirm.text)
	}
	msg := accept(t, m)
	if _, ok := msg.(backendReconciledMsg); !ok {
		t.Fatalf("msg = %#v, want backendReconciledMsg", msg)
	}
	if f.reconciled != "minio-b" {
		t.Errorf("reconciled %q, want minio-b", f.reconciled)
	}
}

// TestDrain_StartsPollsAndFinishes drives a drain from acceptance through a
// progress reading to completion, which is what makes the counts move without
// a stream.
func TestDrain_StartsPollsAndFinishes(t *testing.T) {
	t.Parallel()
	f := &fakeLister{drainProgress: []*adminapi.DrainProgressResponse{
		{Active: true, ObjectsMoved: 4, ObjectsRemaining: 6, BytesRemaining: 2048},
		{Active: false},
	}}
	m := backendsModel(t, f)

	m.handleBackendsKey(key("d"))
	msg := accept(t, m)
	if m.backends.drain.backend != "minio-a" {
		t.Fatalf("watch backend = %q, want minio-a set the moment the drain is accepted", m.backends.drain.backend)
	}

	started, ok := msg.(drainStartedMsg)
	if !ok {
		t.Fatalf("msg = %#v, want drainStartedMsg", msg)
	}
	if f.drainStarted != "minio-a" {
		t.Errorf("StartDrain got %q, want minio-a", f.drainStarted)
	}

	// the accepted drain begins polling
	_, cmd := m.applyDrainStarted(started)
	if cmd == nil {
		t.Fatal("a started drain should schedule a poll")
	}
	if !m.backends.drain.ticking {
		t.Error("the poll ticker should be running")
	}

	// an active reading renders the counts
	m.applyDrainProgress(drainProgressMsg{backend: "minio-a", progress: f.drainProgress[0]})
	if line := m.drainProgressLine(); !strings.Contains(line, "moved 4") || !strings.Contains(line, "remaining 6") {
		t.Errorf("progress line = %q, want the moved and remaining counts", line)
	}

	// an inactive reading ends the watch and refreshes the snapshot
	_, done := m.applyDrainProgress(drainProgressMsg{backend: "minio-a", progress: &adminapi.DrainProgressResponse{}})
	if m.backends.drain.backend != "" {
		t.Error("a finished drain should end the watch")
	}
	if done == nil {
		t.Error("a finished drain should refresh the status snapshot")
	}
	if m.status == nil || !m.status.ok {
		t.Errorf("status = %+v, want a successful completion line", m.status)
	}
}

// TestDrain_StaleProgressIgnored asserts a reading for a drain the pane is no
// longer following cannot resurrect the watch.
func TestDrain_StaleProgressIgnored(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	m.backends.drain = drainWatch{backend: "minio-a"}

	m.applyDrainProgress(drainProgressMsg{
		backend:  "minio-b",
		progress: &adminapi.DrainProgressResponse{Active: true, ObjectsMoved: 99},
	})
	if m.backends.drain.progress != nil {
		t.Error("a reading for another backend should be ignored")
	}
}

// TestDrain_StartFailureClearsTheWatch asserts a drain that never started does
// not leave the pane claiming to follow one.
func TestDrain_StartFailureClearsTheWatch(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	m.beginDrainWatch("minio-a")

	m.applyDrainStarted(drainStartedMsg{backend: "minio-a", err: errors.New("backend busy")})
	if m.backends.drain.backend != "" {
		t.Error("a failed start should clear the watch")
	}
	if m.status == nil || m.status.ok {
		t.Errorf("status = %+v, want a failure line", m.status)
	}
}

// TestDrain_CancelOnlyWhileFollowing asserts the cancel key does nothing
// unless this pane is following a drain on the highlighted backend.
func TestDrain_CancelOnlyWhileFollowing(t *testing.T) {
	t.Parallel()
	f := &fakeLister{}
	m := backendsModel(t, f)

	m.handleBackendsKey(key("x"))
	if m.confirm != nil {
		t.Fatal("cancel armed with no drain in flight")
	}

	m.backends.drain = drainWatch{backend: "minio-a"}
	m.handleBackendsKey(key("x"))
	if m.confirm == nil || !strings.Contains(m.confirm.text, "minio-a") {
		t.Fatalf("confirm = %+v, want a cancel naming minio-a", m.confirm)
	}
	if msg := accept(t, m); f.drainCancelled != "minio-a" {
		t.Errorf("CancelDrain got %q (msg %#v), want minio-a", f.drainCancelled, msg)
	}
}

// TestDrain_TickLapsesOffPane asserts the poll loop stops once the operator
// navigates away, rather than polling forever in the background.
func TestDrain_TickLapsesOffPane(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	m.backends.drain = drainWatch{backend: "minio-a", ticking: true}
	m.section = sectionFiles

	if _, cmd := m.onDrainTick(); cmd != nil {
		t.Error("the ticker should lapse once the pane is not active")
	}
	if m.backends.drain.ticking {
		t.Error("ticking should be cleared when the ticker lapses")
	}
}

// TestBackendActions_ReportOutcomes pins what each one-shot action says when it
// succeeds and when it fails.
func TestBackendActions_ReportOutcomes(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})

	m.applyBackendReconciled(backendReconciledMsg{
		backend: "minio-a",
		resp:    &adminapi.ReconcileResponse{Imported: 3, Removed: 1},
	})
	if m.status == nil || !strings.Contains(m.status.text, "imported 3") {
		t.Errorf("status = %+v, want the reconcile counts", m.status)
	}

	m.applyBackendRequeued(backendRequeuedMsg{
		backend: "minio-a",
		resp:    &adminapi.CleanupDLQRequeueResponse{Requeued: 7},
	})
	if m.status == nil || !strings.Contains(m.status.text, "7") {
		t.Errorf("status = %+v, want the requeued count", m.status)
	}

	m.applyBackendRequeued(backendRequeuedMsg{backend: "minio-a", err: errors.New("db down")})
	if m.status == nil || m.status.ok {
		t.Errorf("status = %+v, want a failure line", m.status)
	}
}

// TestBackendsFooter_OffersCancelWhileDraining asserts the cancel key is only
// advertised when there is a drain to stop.
func TestBackendsFooter_OffersCancelWhileDraining(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	if strings.Contains(m.backendsFooterView(), "cancel drain") {
		t.Error("cancel offered with no drain in flight")
	}

	m.backends.drain = drainWatch{backend: "minio-a"}
	if !strings.Contains(m.backendsFooterView(), "cancel drain") {
		t.Error("cancel not offered while a drain is being followed")
	}
}

// TestDrain_CancelOutcomes asserts a cancelled drain ends the watch and
// refreshes the snapshot, and that a refused cancel leaves the watch alone so
// the operator can try again.
func TestDrain_CancelOutcomes(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	m.backends.drain = drainWatch{backend: "minio-a"}

	_, cmd := m.applyDrainCancelled(drainCancelledMsg{backend: "minio-a", err: errors.New("no drain in progress")})
	if m.backends.drain.backend == "" {
		t.Error("a refused cancel should leave the watch in place")
	}
	if cmd != nil {
		t.Error("a refused cancel should not refresh the snapshot")
	}
	if m.status == nil || m.status.ok {
		t.Errorf("status = %+v, want a failure line", m.status)
	}

	_, cmd = m.applyDrainCancelled(drainCancelledMsg{backend: "minio-a"})
	if m.backends.drain.backend != "" {
		t.Error("a successful cancel should end the watch")
	}
	if cmd == nil {
		t.Error("a successful cancel should refresh the snapshot")
	}
}

// TestDrainProgressLine_States pins what the header reports at each stage of a
// drain, including the poll failure an operator needs to see.
func TestDrainProgressLine_States(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})

	if got := m.drainProgressLine(); got != "" {
		t.Errorf("line = %q, want nothing while idle", got)
	}

	m.backends.drain = drainWatch{backend: "minio-a"}
	if got := m.drainProgressLine(); !strings.Contains(got, "starting") {
		t.Errorf("line = %q, want a starting notice before the first poll", got)
	}

	m.backends.drain.err = errors.New("connection refused")
	if got := m.drainProgressLine(); !strings.Contains(got, "connection refused") {
		t.Errorf("line = %q, want the poll failure surfaced", got)
	}
}

// TestSelectedBackend_EmptyTable asserts an action on an empty pane is a no-op
// rather than an index panic.
func TestSelectedBackend_EmptyTable(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionBackends

	if got := m.selectedBackend(); got != "" {
		t.Errorf("selectedBackend = %q, want empty", got)
	}
	if _, cmd := m.handleBackendsKey(key("d")); cmd != nil || m.confirm != nil {
		t.Error("an action on an empty pane should do nothing")
	}
}

// TestDrainProgress_PollErrorKeepsWatching asserts one failed poll reports
// itself without abandoning the drain, since the next tick may well succeed.
func TestDrainProgress_PollErrorKeepsWatching(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	m.backends.drain = drainWatch{backend: "minio-a", ticking: true}

	m.applyDrainProgress(drainProgressMsg{backend: "minio-a", err: errors.New("timeout")})
	if m.backends.drain.backend != "minio-a" {
		t.Error("a failed poll should not end the watch")
	}
	if m.backends.drain.err == nil {
		t.Error("a failed poll should be recorded for the header")
	}
}

// TestDrain_TickContinuesWhileActive asserts the poll loop reschedules itself
// while the drain is still running and the pane is still in view.
func TestDrain_TickContinuesWhileActive(t *testing.T) {
	t.Parallel()
	f := &fakeLister{drainProgress: []*adminapi.DrainProgressResponse{{Active: true, ObjectsMoved: 1}}}
	m := backendsModel(t, f)
	m.backends.drain = drainWatch{backend: "minio-a", ticking: true}

	_, cmd := m.onDrainTick()
	if cmd == nil {
		t.Fatal("an active drain on the visible pane should keep polling")
	}
	if cmd() == nil {
		t.Error("the scheduled poll produced no message")
	}
}

// TestBackendActions_ReportFailures asserts a failed pass says so rather than
// reporting counts it never got.
func TestBackendActions_ReportFailures(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})

	if _, cmd := m.applyBackendReconciled(backendReconciledMsg{
		backend: "minio-a", err: errors.New("backend unreachable"),
	}); cmd != nil {
		t.Error("a failed reconcile should not refresh the snapshot")
	}
	if m.status == nil || m.status.ok || !strings.Contains(m.status.text, "unreachable") {
		t.Errorf("status = %+v, want the failure surfaced", m.status)
	}
}

// TestRequeueBackendDLQ_ScopesToRow asserts the requeue command carries the
// highlighted backend through to the client.
func TestRequeueBackendDLQ_ScopesToRow(t *testing.T) {
	t.Parallel()
	f := &fakeLister{requeued: &adminapi.CleanupDLQRequeueResponse{Backend: "minio-a", Requeued: 2}}
	m := backendsModel(t, f)

	m.handleBackendsKey(key("Q"))
	msg := accept(t, m)
	requeued, ok := msg.(backendRequeuedMsg)
	if !ok {
		t.Fatalf("msg = %#v, want backendRequeuedMsg", msg)
	}
	if requeued.backend != "minio-a" || requeued.resp.Requeued != 2 {
		t.Errorf("msg = %+v, want minio-a with 2 requeued", requeued)
	}
}

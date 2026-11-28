// -------------------------------------------------------------------------------
// TUI - Replication View Tests
//
// Author: Alex Freidah
//
// Covers the replication pane's load commands, snapshot/error transitions, the
// self-perpetuating auto-refresh ticker (runs while active, lapses on leave),
// and the rendered summary across factor, bucket factor, backlog, and
// disabled states.
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
// CONSTANTS
// -------------------------------------------------------------------------

// errNope is a canned failure for the error transitions.
var errNope = errors.New("nope")

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestLoadReplication covers both delivery paths of the fetch command.
func TestLoadReplication(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{replic: &adminapi.ReplicationStatusResponse{Factor: 2}})
	m.Update(m.loadReplication()())
	if m.replication.snap == nil || m.replication.snap.Factor != 2 {
		t.Errorf("snap = %+v, want the served snapshot", m.replication.snap)
	}
	m = initialModel(errLister{})
	m.Update(m.loadReplication()())
	if m.replication.err == nil {
		t.Error("a failed first fetch should surface on the pane")
	}
}

// TestApplyReplicationErr keeps the last snapshot on a transient refresh error
// but surfaces the error when nothing has loaded yet.
func TestApplyReplicationErr(t *testing.T) {
	t.Parallel()
	// no snapshot yet: the error surfaces.
	m := initialModel(&fakeLister{})
	m.applyReplicationErr(errNope)
	if m.replication.err == nil {
		t.Error("first-load error should surface")
	}
	// with a snapshot on screen: the error is swallowed, snapshot kept.
	m = initialModel(&fakeLister{})
	m.applyReplication(&adminapi.ReplicationStatusResponse{Factor: 2})
	m.applyReplicationErr(errNope)
	if m.replication.err != nil || m.replication.snap == nil {
		t.Errorf("refresh error should be swallowed: err=%v snap=%v", m.replication.err, m.replication.snap)
	}
}

// TestEnterReplication fetches on the first visit with the spinner showing,
// does not send a second request while the first is in flight, and shows no
// spinner on a later visit once a snapshot exists.
func TestEnterReplication(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{replic: &adminapi.ReplicationStatusResponse{Factor: 2}})
	_, first := m.selectSection(sectionReplication)
	if first == nil {
		t.Fatal("first enter: expected a fetch")
	}
	if !m.replication.loading {
		t.Error("first enter should show the spinner")
	}
	if _, cmd := m.selectSection(sectionReplication); cmd != nil {
		t.Error("re-entering while the first fetch is in flight should not send another")
	}

	m.Update(first())
	if _, cmd := m.selectSection(sectionReplication); cmd == nil {
		t.Error("re-entering after the fetch landed should refresh")
	}
	if m.replication.loading {
		t.Error("re-entering with a snapshot should not show the spinner")
	}
}

// TestHandleReplicationKey covers back-to-nav and forced reload.
func TestHandleReplicationKey(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionReplication
	if _, _ = m.handlePaneKey(tea.KeyMsg{Type: tea.KeyEsc}); !m.navFocus {
		t.Error("esc should return focus to the nav")
	}
	if _, cmd := m.handlePaneKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); cmd == nil {
		t.Error("r should issue a reload command")
	}
}

// TestReplicationBody renders each state.
func TestReplicationBody(t *testing.T) {
	t.Parallel()
	if got := bodyText((&model{replication: replicationView{err: errNope}}).replicationBody()); !strings.Contains(got, "nope") {
		t.Errorf("error body = %q", got)
	}
	if got := bodyText((&model{replication: replicationView{snap: nil}}).replicationBody()); !strings.Contains(got, "no replication data") {
		t.Errorf("empty body = %q", got)
	}
	m := &model{replication: replicationView{snap: &adminapi.ReplicationStatusResponse{
		Factor: 2, UnderReplicated: 143, OverReplicated: 12, ComputedAt: time.Now(),
	}}}
	got := bodyText(m.replicationBody())
	for _, want := range []string{"factor", "143", "under-replicated", "12", "over-replicated", "read at"} {
		if !strings.Contains(got, want) {
			t.Errorf("stats body %q missing %q", got, want)
		}
	}
}

// TestReplicationStats_Disabled renders the disabled notice when no key's
// factor leaves anything to replicate.
func TestReplicationStats_Disabled(t *testing.T) {
	t.Parallel()
	m := &model{replication: replicationView{snap: &adminapi.ReplicationStatusResponse{Factor: 1}}}
	if got := m.replicationStats(); !strings.Contains(got, "disabled") {
		t.Errorf("factor 1 stats = %q, want disabled notice", got)
	}
}

// TestReplicationStats_BucketFactors verifies a deployment at factor 1 whose
// virtual buckets set their own still shows the backlog, with each bucket and
// its factor listed in name order under one label.
func TestReplicationStats_BucketFactors(t *testing.T) {
	t.Parallel()
	snap := &adminapi.ReplicationStatusResponse{Replicating: true, UnderReplicated: 7, ComputedAt: time.Now()}
	snap.Factor, snap.BucketFactors = 1, map[string]int{"wal": 3, "photos": 2}
	got := (&model{replication: replicationView{snap: snap}}).replicationStats()
	assertContains(t, got, "bucket factors", "photos  2", "wal     3", "7 objects")
	if strings.Contains(got, "disabled") {
		t.Errorf("stats = %q, want no disabled notice while a bucket replicates", got)
	}
	if strings.Index(got, "photos") > strings.Index(got, "wal") || strings.Count(got, "bucket factors") != 1 {
		t.Errorf("stats = %q, want the buckets in name order under one label", got)
	}
}

// TestReplicationReadAt_ZeroTimeIsUnknown covers a response with no read time,
// which has none to report.
func TestReplicationReadAt_ZeroTimeIsUnknown(t *testing.T) {
	t.Parallel()
	if got := replicationReadAt(time.Time{}); got != "unknown" {
		t.Errorf("zero-time read = %q, want unknown", got)
	}
}

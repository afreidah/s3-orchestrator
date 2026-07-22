// -------------------------------------------------------------------------------
// TUI - Replication View Tests
//
// Author: Alex Freidah
//
// Covers the replication pane's load commands, snapshot/error transitions, the
// self-perpetuating auto-refresh ticker (runs while active, lapses on leave),
// and the rendered summary across factor, backlog, and disabled states.
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

// errNope is a canned failure for the error transitions.
var errNope = errors.New("nope")

// TestLoadReplication covers both delivery paths of the fetch command.
func TestLoadReplication(t *testing.T) {
	t.Parallel()
	ok := initialModel(&fakeLister{replic: &adminapi.ReplicationStatusResponse{Factor: 2}}).loadReplication()
	if _, isMsg := ok().(replicationLoadedMsg); !isMsg {
		t.Errorf("success cmd = %#v, want replicationLoadedMsg", ok())
	}
	fail := initialModel(errLister{}).loadReplication()
	if _, isMsg := fail().(replicationErrMsg); !isMsg {
		t.Errorf("error cmd = %#v, want replicationErrMsg", fail())
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

// TestEnterReplication starts the ticker once and does not double-start it on a
// repeat visit while the previous ticker is still live.
func TestEnterReplication(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	if _, cmd := m.enterReplication(); cmd == nil {
		t.Fatal("first enter: expected load + tick batch")
	}
	if !m.replication.ticking || !m.replication.loading {
		t.Errorf("first enter: ticking=%v loading=%v, want both true", m.replication.ticking, m.replication.loading)
	}
	// a snapshot arrives, then a repeat visit: no spinner, ticker still marked.
	m.applyReplication(&adminapi.ReplicationStatusResponse{Factor: 2})
	m.enterReplication()
	if m.replication.loading {
		t.Error("repeat enter with a snapshot should not show the spinner")
	}
	if !m.replication.ticking {
		t.Error("repeat enter should keep the ticker running")
	}
}

// TestOnReplicationTick reschedules while the pane is active and lapses once the
// user has navigated away.
func TestOnReplicationTick(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionReplication
	m.replication.ticking = true
	if _, cmd := m.onReplicationTick(); cmd == nil {
		t.Error("active tick: expected a reschedule command")
	}

	m.section = sectionBackends
	if _, cmd := m.onReplicationTick(); cmd != nil || m.replication.ticking {
		t.Errorf("inactive tick: cmd=%v ticking=%v, want nil + stopped", cmd, m.replication.ticking)
	}
}

// TestHandleReplicationKey covers back-to-nav and forced reload.
func TestHandleReplicationKey(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionReplication
	if _, _ = m.handleReplicationKey(tea.KeyMsg{Type: tea.KeyEsc}); !m.navFocus {
		t.Error("esc should return focus to the nav")
	}
	if _, cmd := m.handleReplicationKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); cmd == nil {
		t.Error("r should issue a reload command")
	}
}

// TestReplicationBodyView renders each state.
func TestReplicationBodyView(t *testing.T) {
	t.Parallel()
	if got := (&model{replication: replicationView{err: errNope}}).replicationBodyView(); !strings.Contains(got, "nope") {
		t.Errorf("error body = %q", got)
	}
	if got := (&model{replication: replicationView{snap: nil}}).replicationBodyView(); !strings.Contains(got, "no replication data") {
		t.Errorf("empty body = %q", got)
	}
	m := &model{replication: replicationView{snap: &adminapi.ReplicationStatusResponse{
		Factor: 2, UnderReplicated: 143, OverReplicated: 12, ComputedAt: time.Now(),
	}}}
	got := m.replicationBodyView()
	for _, want := range []string{"factor", "143", "under-replicated", "12", "over-replicated", "ago"} {
		if !strings.Contains(got, want) {
			t.Errorf("stats body %q missing %q", got, want)
		}
	}
}

// TestReplicationStats_Disabled renders the disabled notice when factor <= 1.
func TestReplicationStats_Disabled(t *testing.T) {
	t.Parallel()
	m := &model{replication: replicationView{snap: &adminapi.ReplicationStatusResponse{Factor: 1}}}
	if got := m.replicationStats(); !strings.Contains(got, "disabled") {
		t.Errorf("factor<=1 stats = %q, want disabled notice", got)
	}
}

// TestHumanDuration covers the second/minute/hour rounding boundaries.
func TestHumanDuration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m"},
		{3 * time.Hour, "3h"},
	}
	for _, c := range cases {
		if got := humanDuration(c.in); got != c.want {
			t.Errorf("humanDuration(%s) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := replicationAge(time.Time{}); got != "unknown" {
		t.Errorf("zero-time age = %q, want unknown", got)
	}
}

// -------------------------------------------------------------------------------
// TUI - Replication View
//
// Author: Alex Freidah
//
// Read-only pane over the cluster's replication health, fetched from the admin
// replication endpoint: the configured factor and the current under- and
// over-replicated object counts. The pane auto-refreshes on a ticker while it
// is the active section (the counts drift constantly as workers reconcile), so
// entering it starts a self-perpetuating tick that lapses once the user leaves.
// Reached with "p"; "esc" returns focus to the nav, "r" forces a refresh.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// replicationRefreshInterval is how often the pane re-fetches the snapshot while
// it is the active section.
const replicationRefreshInterval = 3 * time.Second

// replicationView holds the state of the replication status pane.
type replicationView struct {
	snap    *adminapi.ReplicationStatusResponse // last snapshot, nil until the first load
	loading bool                                // a fetch is in flight (only shown on the first load)
	ticking bool                                // the auto-refresh ticker is running
	err     error                               // last fetch error, surfaced only when there is no snapshot yet
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// replicationLoadedMsg carries a successfully loaded replication snapshot.
type replicationLoadedMsg struct {
	resp *adminapi.ReplicationStatusResponse
}

// replicationErrMsg carries a failed replication fetch.
type replicationErrMsg struct{ err error }

// replicationTickMsg fires on the auto-refresh interval while the pane is active.
type replicationTickMsg struct{}

// loadReplication returns a command that fetches the replication snapshot off
// the main loop, delivering a replicationLoadedMsg or replicationErrMsg.
func (m *model) loadReplication() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.GetReplicationStatus(context.Background())
		if err != nil {
			return replicationErrMsg{err}
		}
		return replicationLoadedMsg{resp}
	}
}

// replicationTick schedules the next auto-refresh tick.
func (m *model) replicationTick() tea.Cmd {
	return tea.Tick(replicationRefreshInterval, func(time.Time) tea.Msg { return replicationTickMsg{} })
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// enterReplication kicks off the first (or a repeat) load and, if not already
// running, starts the auto-refresh ticker. The last snapshot is kept across
// visits so re-entering shows data immediately instead of a spinner flash.
func (m *model) enterReplication() (tea.Model, tea.Cmd) {
	m.replication.loading = m.replication.snap == nil
	cmds := []tea.Cmd{m.loadReplication()}
	if !m.replication.ticking {
		m.replication.ticking = true
		cmds = append(cmds, m.replicationTick())
	}
	return m, tea.Batch(cmds...)
}

// onReplicationTick refreshes the snapshot and reschedules, but only while the
// pane is still active; once the user has navigated away the ticker lapses.
func (m *model) onReplicationTick() (tea.Model, tea.Cmd) {
	if m.section != sectionReplication {
		m.replication.ticking = false
		return m, nil
	}
	return m, tea.Batch(m.loadReplication(), m.replicationTick())
}

// applyReplication folds a loaded snapshot into the pane state.
func (m *model) applyReplication(resp *adminapi.ReplicationStatusResponse) {
	m.replication.snap = resp
	m.replication.loading = false
	m.replication.err = nil
}

// applyReplicationErr records a fetch failure. A transient auto-refresh error is
// swallowed while a prior snapshot is on screen; the error only surfaces when
// there is nothing to show yet.
func (m *model) applyReplicationErr(err error) {
	m.replication.loading = false
	if m.replication.snap == nil {
		m.replication.err = err
	}
}

// handleReplicationKey applies replication-pane keys (back, reload); scrolling
// is unused because the pane is a fixed summary.
func (m *model) handleReplicationKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "left", "h":
		return m.navBack()
	case "r":
		m.replication.loading = m.replication.snap == nil
		cmd := m.loadReplication()
		return m, cmd
	}
	return m, nil
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// replicationPaneView composes the pane's full-screen layout.
func (m *model) replicationPaneView() string {
	return m.frame(m.replicationHeaderView(), m.replicationFooterView(), m.replicationBodyView())
}

// replicationHeaderView renders the title bar with the auto-refresh cadence.
func (m *model) replicationHeaderView() string {
	title := fmt.Sprintf("replication   auto-refresh %ds", int(replicationRefreshInterval/time.Second))
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title)
}

// replicationFooterView renders the replication key hints.
func (m *model) replicationFooterView() string {
	return m.footer("r reload - tab nav - q quit")
}

// replicationBodyView renders the current content: an error, the first-load
// indicator, an empty notice, or the snapshot summary.
func (m *model) replicationBodyView() string {
	return m.paneBody(m.replication.err, "", m.replication.loading, func() string {
		if m.replication.snap == nil {
			return pathStyle.Render("(no replication data)")
		}
		return m.replicationStats()
	})
}

// replicationStats renders the snapshot as an aligned label/value block, with
// the pending counts coloured so a non-zero backlog stands out.
func (m *model) replicationStats() string {
	s := m.replication.snap
	const labelW = 18
	line := func(label, value string) string {
		return fmt.Sprintf("%-*s %s", labelW, label, value)
	}
	factor := line("factor", fmt.Sprintf("%d", s.Factor))
	if s.Factor <= 1 {
		return factor + "\n" + pathStyle.Render("(replication disabled)")
	}
	under := line("under-replicated", replCountStyle(s.UnderReplicated).Render(fmt.Sprintf("%d objects", s.UnderReplicated)))
	over := line("over-replicated", replCountStyle(s.OverReplicated).Render(fmt.Sprintf("%d objects", s.OverReplicated)))
	age := line("snapshot age", pathStyle.Render(replicationAge(s.ComputedAt)))
	return factor + "\n" + under + "\n" + over + "\n" + age
}

// replCountStyle colours a pending count: green at zero (nothing to do), amber
// when there is a backlog.
func replCountStyle(n int64) lipgloss.Style {
	if n == 0 {
		return statusOKStyle
	}
	return logLevelWarn
}

// replicationAge renders how stale the snapshot is, next to the wall-clock time
// it was computed. The snapshot is recomputed on the collector's own cycle, so
// this reflects server-side freshness rather than the poll interval.
func replicationAge(computed time.Time) string {
	if computed.IsZero() {
		return "unknown"
	}
	d := max(time.Since(computed), 0)
	return fmt.Sprintf("%s ago (%s)", humanDuration(d), computed.Format("15:04:05"))
}

// humanDuration renders a short, coarse age: seconds under a minute, then
// minutes, then hours.
func humanDuration(d time.Duration) string {
	return humanize.Duration(d)
}

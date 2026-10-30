// -------------------------------------------------------------------------------
// TUI - Replication View
//
// Author: Alex Freidah
//
// Read-only pane over the cluster's replication health, fetched from the admin
// replication endpoint: the configured factor and the current under- and
// over-replicated object counts. The shared poller keeps the snapshot fresh
// (the counts drift constantly as workers reconcile), so the pane is current
// whenever it is opened. Reached with "p"; "esc" returns focus to the nav, "r"
// forces a refresh.
// -------------------------------------------------------------------------------

package tui

import (
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	tea "github.com/charmbracelet/bubbletea"
)

// replicationView holds the state of the replication status pane. Its error is
// surfaced only when there is no snapshot yet.
type replicationView struct {
	loadState
	snap *adminapi.ReplicationStatusResponse // last snapshot, nil until the first load
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// loadReplication returns a command that fetches the replication snapshot off
// the main loop.
func (m *model) loadReplication() tea.Cmd {
	return fetchCmd(m.client.GetReplicationStatus)
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyReplication folds a loaded snapshot into the pane state.
func (m *model) applyReplication(resp *adminapi.ReplicationStatusResponse) {
	m.replication.snap = resp
	m.replication.loaded()
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

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// replicationHeaderView renders the title bar with the auto-refresh cadence.
func (m *model) replicationHeaderView() string {
	title := fmt.Sprintf("replication   auto-refresh %ds", int(pollIntervals[pollReplication]/time.Second))
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title)
}

// replicationBody renders the current content: an error, the first-load
// indicator, an empty notice, or the snapshot summary.
func (m *model) replicationBody() []pane {
	return m.paneBody(m.replication.err, "", m.replication.loading, func() []pane {
		if m.replication.snap == nil {
			return []pane{textPane(pathStyle.Render("(no replication data)"))}
		}
		return []pane{textPane(m.replicationStats())}
	})
}

// replicationStats renders the snapshot as an aligned label/value block, with
// the pending counts coloured so a non-zero backlog stands out.
func (m *model) replicationStats() string {
	s := m.replication.snap
	const labelW = 18
	factor := field{"factor", fmt.Sprintf("%d", s.Factor)}
	if s.Factor <= 1 {
		return fieldBlock(labelW, factor) + "\n" + pathStyle.Render("(replication disabled)")
	}
	return fieldBlock(labelW,
		factor,
		field{"under-replicated", backlogStyle(s.UnderReplicated).Render(fmt.Sprintf("%d objects", s.UnderReplicated))},
		field{"over-replicated", backlogStyle(s.OverReplicated).Render(fmt.Sprintf("%d objects", s.OverReplicated))},
		field{"snapshot age", pathStyle.Render(replicationAge(s.ComputedAt))},
	)
}

// replicationAge renders how stale the snapshot is, next to the wall-clock time
// it was computed. The snapshot is recomputed on the collector's own cycle, so
// this reflects server-side freshness rather than the poll interval.
func replicationAge(computed time.Time) string {
	if computed.IsZero() {
		return "unknown"
	}
	d := max(time.Since(computed), 0)
	return fmt.Sprintf("%s ago (%s)", humanize.Duration(d), computed.Format("15:04:05"))
}

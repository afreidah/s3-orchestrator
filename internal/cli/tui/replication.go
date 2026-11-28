// -------------------------------------------------------------------------------
// TUI - Replication View
//
// Author: Alex Freidah
//
// Read-only pane over the cluster's replication health, fetched from the admin
// replication endpoint: the deployment's factor, each virtual bucket setting
// a factor of its own, and the current under- and over-replicated object
// counts. The shared poller keeps the snapshot fresh (the counts drift
// constantly as workers reconcile), so the pane is current whenever it is
// opened. Reached with "p"; "esc" returns focus to the nav, "r" forces a
// refresh.
// -------------------------------------------------------------------------------

package tui

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
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
	return fetchCmd(pollReplication, m.client.GetReplicationStatus, (*model).applyReplication, (*model).applyReplicationErr)
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
		m.replication.failed(err)
	}
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// replicationHeaderView renders the title bar with the auto-refresh cadence.
func (m *model) replicationHeaderView() string {
	return m.titleBar(fmt.Sprintf("replication   auto-refresh %ds", int(pollSources[pollReplication].every/time.Second)))
}

// replicationBody renders the current content: an error, the first-load
// indicator, an empty notice, or the snapshot summary.
func (m *model) replicationBody() []pane {
	return m.paneBody(&m.replication.loadState, func() []pane {
		if m.replication.snap == nil {
			return []pane{textPane(pathStyle.Render("(no replication data)"))}
		}
		return []pane{textPane(m.replicationStats())}
	})
}

// replicationStats renders the snapshot as an aligned label/value block: the
// deployment's factor, each virtual bucket at a factor of its own, and the
// pending counts, coloured so a non-zero backlog stands out.
func (m *model) replicationStats() string {
	s := m.replication.snap
	const labelW = 18
	fields := append([]field{{"factor", strconv.Itoa(s.Factor)}}, bucketFactorFields(s.BucketFactors)...)
	if !replicating(s) {
		return fieldBlock(labelW, fields...) + "\n" + pathStyle.Render("(replication disabled)")
	}
	return fieldBlock(labelW, append(fields,
		field{"under-replicated", backlogStyle(s.UnderReplicated).Render(fmt.Sprintf("%d objects", s.UnderReplicated))},
		field{"over-replicated", backlogStyle(s.OverReplicated).Render(fmt.Sprintf("%d objects", s.OverReplicated))},
		field{"read at", pathStyle.Render(replicationReadAt(s.ComputedAt))},
	)...)
}

// bucketFactorFields lists each virtual bucket setting a factor of its own, in
// name order with the factors aligned, under a single label.
func bucketFactorFields(factors map[string]int) []field {
	names := slices.Sorted(maps.Keys(factors))
	width := 0
	for _, name := range names {
		width = max(width, len(name))
	}
	out := make([]field, len(names))
	for i, name := range names {
		out[i].value = fmt.Sprintf("%-*s  %d", width, name, factors[name])
	}
	if len(out) > 0 {
		out[0].label = "bucket factors"
	}
	return out
}

// replicating reports whether the snapshot's factors leave the workers
// anything to keep. An instance that does not report it replicates exactly
// when its factor is above one.
func replicating(s *adminapi.ReplicationStatusResponse) bool {
	return s.Replicating || s.Factor > 1
}

// ownFactorsText says how many virtual buckets set a factor of their own.
func ownFactorsText(n int) string {
	return humanize.Count(int64(n), "bucket sets its own", "buckets set their own")
}

// replicationReadAt renders the wall-clock time the server read the counts, so
// a stalled poll shows as a time that stops moving.
func replicationReadAt(read time.Time) string {
	if read.IsZero() {
		return "unknown"
	}
	return read.Format("15:04:05")
}

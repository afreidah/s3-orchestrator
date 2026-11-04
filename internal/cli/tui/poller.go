// -------------------------------------------------------------------------------
// TUI - Shared Poller
//
// Author: Alex Freidah
//
// One refresh loop for every pane. A single tick runs once a second and asks
// each poll target whether it is due: its interval has passed since it was
// last requested, no request for it is still in flight, and it is wanted right
// now. pollSources declares all of that per target, and each target is fetched
// by its pane's load command.
//
// Targets whose data is fleet-wide (status, replication, cleanup, buckets, and
// a drain being followed) are polled on every pane, so a pane is current the
// moment it is opened and a drain keeps being followed while the operator
// looks elsewhere. Targets whose data belongs to the instance that answered
// (workers, cache, logs) are polled only while their pane is showing: behind a
// load balancer, background polls would mix readings from different instances.
// Logs are polled only while the operator follows them, since each poll
// replaces what is being read. The file listing is never polled, because it is
// being navigated.
// -------------------------------------------------------------------------------

package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// pollTarget is one endpoint the poller keeps fresh.
type pollTarget int

// The poll targets, in the order the poller considers them.
const (
	pollStatus pollTarget = iota
	pollReplication
	pollCleanup
	pollBuckets
	pollWorkers
	pollCache
	pollDrain
	pollLogs
	pollTargetCount
)

// noPoll marks a fetch the poller did not send and does not track.
const noPoll pollTarget = -1

// pollTickInterval is how often the poller checks which targets are due. It
// bounds how late a target can be, not how often anything is fetched.
const pollTickInterval = time.Second

// pollSource is one target's refresh: how long it waits between requests,
// whether it is wanted right now (always, when nil), and the command that
// fetches it.
type pollSource struct {
	every  time.Duration
	wanted func(*model) bool
	load   func(*model) tea.Cmd
}

// pollSources declares every target.
var pollSources = [pollTargetCount]pollSource{
	pollStatus:      {every: 10 * time.Second, load: (*model).loadStatus},
	pollReplication: {every: 3 * time.Second, load: (*model).loadReplication},
	pollCleanup:     {every: 10 * time.Second, load: (*model).loadCleanup},
	pollBuckets:     {every: 30 * time.Second, load: (*model).loadBuckets},
	pollWorkers:     {every: 5 * time.Second, wanted: showing(sectionWorkers), load: (*model).loadWorkers},
	pollCache:       {every: 5 * time.Second, wanted: showing(sectionCache), load: (*model).loadCache},
	pollDrain: {
		every:  2 * time.Second,
		wanted: func(m *model) bool { return m.backends.drain.following },
		load:   (*model).loadDrain,
	},
	pollLogs: {
		every:  2 * time.Second,
		wanted: func(m *model) bool { return m.section == sectionLogs && m.logs.following },
		load:   (*model).loadLogs,
	},
}

// showing wants a target only while its pane is on screen.
func showing(s section) func(*model) bool {
	return func(m *model) bool { return m.section == s }
}

// poller records, per target, when it was last requested and whether that
// request is still in flight. The zero value is ready to use: every target is
// due on the first tick.
type poller struct {
	last     [pollTargetCount]time.Time
	inFlight [pollTargetCount]bool
}

// pollTickMsg is the poller's once-a-second tick.
type pollTickMsg struct{ now time.Time }

// pollTick schedules the next tick.
func pollTick() tea.Cmd {
	return tea.Tick(pollTickInterval, func(t time.Time) tea.Msg { return pollTickMsg{now: t} })
}

// onPollTick fetches every target that is due and schedules the next tick.
func (m *model) onPollTick(now time.Time) (tea.Model, tea.Cmd) {
	cmds := []tea.Cmd{pollTick()}
	for t := range pollTargetCount {
		if !m.pollWanted(t) || m.poll.inFlight[t] || now.Sub(m.poll.last[t]) < pollSources[t].every {
			continue
		}
		if cmd := m.fetchAt(t, now); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

// pollWanted reports whether a target should be kept fresh right now.
func (m *model) pollWanted(t pollTarget) bool {
	wanted := pollSources[t].wanted
	return wanted == nil || wanted(m)
}

// fetch requests a target now, for a pane being opened or an operator
// pressing "r". It returns nil when a request for the target is already in
// flight, so a manual refresh never doubles one the poller sent.
func (m *model) fetch(t pollTarget) tea.Cmd {
	if m.poll.inFlight[t] {
		return nil
	}
	return m.fetchAt(t, time.Now())
}

// fetchAt marks a target in flight and returns its pane's load command.
func (m *model) fetchAt(t pollTarget, now time.Time) tea.Cmd {
	cmd := pollSources[t].load(m)
	if cmd == nil {
		return nil
	}
	m.poll.inFlight[t] = true
	m.poll.last[t] = now
	return cmd
}

// landed clears a target's in-flight mark once its result has arrived.
func (p *poller) landed(t pollTarget) {
	if t != noPoll {
		p.inFlight[t] = false
	}
}

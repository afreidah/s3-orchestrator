// -------------------------------------------------------------------------------
// TUI - Shared Poller
//
// Author: Alex Freidah
//
// One refresh loop for every pane. A single tick runs once a second and asks
// each poll target whether it is due: its interval has passed since it was
// last requested, no request for it is still in flight, and it is wanted right
// now. Each target is fetched by its pane's existing load command, so the
// panes keep applying results exactly as they did when they loaded themselves.
//
// Targets whose data is fleet-wide (status, replication, cleanup, buckets, and
// a drain being followed) are polled on every pane, so a pane is current the
// moment it is opened and a drain keeps being followed while the operator
// looks elsewhere. Targets whose data belongs to the instance that answered
// (workers, cache) are polled only while their pane is showing: behind a load
// balancer, background polls would mix readings from different instances.
// Logs are refreshed only on request, and the file listing is never polled,
// because both are being navigated.
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
	pollTargetCount
)

// pollTickInterval is how often the poller checks which targets are due. It
// bounds how late a target can be, not how often anything is fetched.
const pollTickInterval = time.Second

// pollIntervals is how long each target waits between requests.
var pollIntervals = [pollTargetCount]time.Duration{
	pollStatus:      10 * time.Second,
	pollReplication: 3 * time.Second,
	pollCleanup:     10 * time.Second,
	pollBuckets:     30 * time.Second,
	pollWorkers:     5 * time.Second,
	pollCache:       5 * time.Second,
	pollDrain:       2 * time.Second,
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
		if !m.pollWanted(t) || m.poll.inFlight[t] || now.Sub(m.poll.last[t]) < pollIntervals[t] {
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
	switch t {
	case pollWorkers:
		return m.section == sectionWorkers
	case pollCache:
		return m.section == sectionCache
	case pollDrain:
		return m.backends.drain.following
	default:
		return true
	}
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
	cmd := m.loadCmd(t)
	if cmd == nil {
		return nil
	}
	m.poll.inFlight[t] = true
	m.poll.last[t] = now
	return cmd
}

// loadCmd returns the load command that fetches a target.
func (m *model) loadCmd(t pollTarget) tea.Cmd {
	switch t {
	case pollStatus:
		return m.loadStatus()
	case pollReplication:
		return m.loadReplication()
	case pollCleanup:
		return m.loadCleanup()
	case pollBuckets:
		return m.loadBuckets()
	case pollWorkers:
		return m.loadWorkers()
	case pollCache:
		return m.loadCache()
	case pollDrain:
		if m.backends.drain.backend == "" {
			return nil
		}
		return m.pollDrain(m.backends.drain.backend)
	}
	return nil
}

// pollTargetOf names the target a loaded or failed message answers, so its
// in-flight mark can be cleared. Messages the poller did not send report false.
func pollTargetOf(msg tea.Msg) (pollTarget, bool) {
	switch msg.(type) {
	case statusLoadedMsg, statusErrMsg:
		return pollStatus, true
	case replicationLoadedMsg, replicationErrMsg:
		return pollReplication, true
	case cleanupLoadedMsg, cleanupErrMsg:
		return pollCleanup, true
	case bucketsLoadedMsg, bucketsErrMsg:
		return pollBuckets, true
	case workersLoadedMsg, workersErrMsg:
		return pollWorkers, true
	case cacheLoadedMsg, cacheErrMsg:
		return pollCache, true
	case drainProgressMsg:
		return pollDrain, true
	}
	return 0, false
}

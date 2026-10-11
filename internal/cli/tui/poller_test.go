// -------------------------------------------------------------------------------
// TUI - Shared Poller Tests
//
// Author: Alex Freidah
//
// Covers which targets the poller fetches on a tick, that a request still in
// flight is never sent twice, that each target waits out its interval, that a
// followed drain keeps being read on every pane, and that a refresh leaves the
// operator's selection where it was.
// -------------------------------------------------------------------------------

package tui

import (
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// TestPoller_FirstTickFetchesFleetWideTargets verifies the first tick fetches
// every fleet-wide target, and leaves the per-instance ones and an idle drain
// alone while their panes are not showing.
func TestPoller_FirstTickFetchesFleetWideTargets(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionFiles

	m.onPollTick(time.Now())

	for _, target := range []pollTarget{pollStatus, pollReplication, pollCleanup, pollBuckets} {
		if !m.poll.inFlight[target] {
			t.Errorf("target %d should be fetched on every pane", target)
		}
	}
	for _, target := range []pollTarget{pollWorkers, pollCache, pollDrain, pollLogs} {
		if m.poll.inFlight[target] {
			t.Errorf("target %d should not be fetched while its pane is hidden or idle", target)
		}
	}
}

// TestPoller_PerInstanceTargetsOnlyWhileVisible verifies workers and cache are
// fetched only while their own pane is showing.
func TestPoller_PerInstanceTargetsOnlyWhileVisible(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionWorkers

	m.onPollTick(time.Now())

	if !m.poll.inFlight[pollWorkers] {
		t.Error("workers should be fetched while the Workers pane is showing")
	}
	if m.poll.inFlight[pollCache] {
		t.Error("cache should not be fetched while another pane is showing")
	}
}

// TestPoller_InFlightIsNotSentAgain verifies a tick does not re-request a
// target whose last request has not answered, however long ago it was sent.
func TestPoller_InFlightIsNotSentAgain(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	start := time.Now()
	m.onPollTick(start)
	sent := m.poll.last[pollStatus]

	m.onPollTick(start.Add(time.Minute))

	if !m.poll.last[pollStatus].Equal(sent) {
		t.Error("an in-flight status request was sent a second time")
	}
}

// TestPoller_WaitsOutTheInterval verifies a target that answered is fetched
// again only once its interval has passed.
func TestPoller_WaitsOutTheInterval(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	start := time.Now()
	m.onPollTick(start)
	m.Update(statusLoadedMsg{resp: &adminapi.StatusResponse{}})
	if m.poll.inFlight[pollStatus] {
		t.Fatal("a status result should clear the in-flight mark")
	}

	m.onPollTick(start.Add(pollIntervals[pollStatus] - time.Second))
	if m.poll.inFlight[pollStatus] {
		t.Error("status was fetched again before its interval passed")
	}

	m.onPollTick(start.Add(pollIntervals[pollStatus]))
	if !m.poll.inFlight[pollStatus] {
		t.Error("status should be fetched again once its interval passed")
	}
}

// TestPoller_FollowsADrainOnAnyPane verifies an accepted drain keeps being
// read after the operator leaves the Backends pane, and stops being read once
// a reading shows it has ended.
func TestPoller_FollowsADrainOnAnyPane(t *testing.T) {
	t.Parallel()
	f := &fakeLister{drainProgress: []*adminapi.DrainProgressResponse{{Active: true, ObjectsMoved: 1}}}
	m := backendsModel(t, f)
	m.beginDrainWatch("minio-a")
	m.applyDrainStarted(drainStartedMsg{backend: "minio-a"})
	m.Update(drainProgressMsg{backend: "minio-a", progress: &adminapi.DrainProgressResponse{Active: true}})
	m.section = sectionFiles

	m.onPollTick(time.Now().Add(pollIntervals[pollDrain]))
	if !m.poll.inFlight[pollDrain] {
		t.Fatal("a followed drain should be read while another pane is showing")
	}
	if msg, ok := m.loadCmd(pollDrain)().(drainProgressMsg); !ok || msg.backend != "minio-a" {
		t.Errorf("drain poll = %#v, want a progress reading for minio-a", msg)
	}

	m.Update(drainProgressMsg{backend: "minio-a", progress: &adminapi.DrainProgressResponse{Active: false, State: "drained"}})
	if m.pollWanted(pollDrain) {
		t.Error("a drain that has ended should no longer be polled")
	}
}

// TestPoller_ManualRefreshSkipsAnInFlightRequest verifies "r" does not double a
// request the poller already sent.
func TestPoller_ManualRefreshSkipsAnInFlightRequest(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	if m.fetch(pollStatus) == nil {
		t.Fatal("the first fetch should return a command")
	}
	if m.fetch(pollStatus) != nil {
		t.Error("a second fetch while the first is in flight should return nil")
	}
}

// TestPollTargetOf_MapsEveryResult verifies each loaded and failed message
// clears the in-flight mark of the target that sent it, and that messages the
// poller did not send clear nothing.
func TestPollTargetOf_MapsEveryResult(t *testing.T) {
	t.Parallel()
	cases := map[pollTarget][]any{
		pollStatus:      {statusLoadedMsg{}, statusErrMsg{}},
		pollReplication: {replicationLoadedMsg{}, replicationErrMsg{}},
		pollCleanup:     {cleanupLoadedMsg{}, cleanupErrMsg{}},
		pollBuckets:     {bucketsLoadedMsg{}, bucketsErrMsg{}},
		pollWorkers:     {workersLoadedMsg{}, workersErrMsg{}},
		pollCache:       {cacheLoadedMsg{}, cacheErrMsg{}},
		pollDrain:       {drainProgressMsg{}},
		pollLogs:        {logsLoadedMsg{}, logsErrMsg{}},
	}
	for want, msgs := range cases {
		for _, msg := range msgs {
			if got, ok := pollTargetOf(msg); !ok || got != want {
				t.Errorf("pollTargetOf(%T) = %d, %v; want %d", msg, got, ok, want)
			}
		}
	}
	if _, ok := pollTargetOf(configLoadedMsg{}); ok {
		t.Error("config is not polled, so its result should map to no target")
	}
}

// TestPoller_LogsOnlyWhileFollowed verifies logs are polled only while the
// Logs pane is showing and the operator is following it.
func TestPoller_LogsOnlyWhileFollowed(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionLogs
	if m.pollWanted(pollLogs) {
		t.Error("logs should not be polled until the operator follows them")
	}
	m.handleLogsKey(key("F"))
	if !m.pollWanted(pollLogs) {
		t.Error("followed logs should be polled while their pane shows")
	}
	m.section = sectionFiles
	if m.pollWanted(pollLogs) {
		t.Error("followed logs should not be polled while another pane shows")
	}
}

// TestPoller_DrainWithNoBackendSendsNothing verifies a drain fetch with no
// backend being followed returns no command and marks nothing in flight.
func TestPoller_DrainWithNoBackendSendsNothing(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	if m.fetch(pollDrain) != nil || m.poll.inFlight[pollDrain] {
		t.Error("a drain fetch with no followed backend should send nothing")
	}
}

// TestRefresh_KeepsTheSelectedBackend verifies a status refresh leaves the
// cursor on the same backend when rows move, and clamps it when that backend
// is gone.
func TestRefresh_KeepsTheSelectedBackend(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	m.backends.list.table.SetCursor(1)

	m.applyStatus(&adminapi.StatusResponse{Backends: []adminapi.BackendStatus{
		{Name: "minio-c"}, {Name: "minio-a"}, {Name: "minio-b"},
	}})
	if got := m.selectedBackend(); got != "minio-b" {
		t.Errorf("selected = %q after rows moved, want minio-b", got)
	}

	m.applyStatus(&adminapi.StatusResponse{Backends: []adminapi.BackendStatus{{Name: "minio-a"}}})
	if got := m.selectedBackend(); got != "minio-a" {
		t.Errorf("selected = %q after the selected backend went away, want the remaining row", got)
	}
}

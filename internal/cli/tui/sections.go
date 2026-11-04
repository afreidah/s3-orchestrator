// -------------------------------------------------------------------------------
// TUI - Section Table
//
// Author: Alex Freidah
//
// Everything a nav destination contributes, declared in one place: its label
// and jump key, its keymap and key handler, how it draws, how it reloads, and
// what entering it does. The nav, the key router, the help overlay and the
// frame all read this table, so a section cannot be half wired.
// -------------------------------------------------------------------------------

package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// section is a top-level nav destination. The zero value is the Dashboard, so
// a new model opens on it.
type section int

const (
	sectionDashboard section = iota
	sectionFiles
	sectionBackends
	sectionBuckets
	sectionReplication
	sectionWorkers
	sectionCleanup
	sectionCache
	sectionConfig
	sectionLogs
	sectionOps
	sectionCount
)

// sectionSpec is one section. keys defaults to reload and back, a nil handle
// ignores pane keys, and a nil header leaves the frame to the file browser,
// which draws its own footer and inspector. enter runs on arrival in place of
// reload. ownsBack marks a pane that gives esc and "r" its own meaning.
type sectionSpec struct {
	label    string
	jump     string
	keys     func(*model) []keyHint
	handle   func(*model, tea.KeyMsg) (tea.Model, tea.Cmd)
	header   func(*model) string
	body     func(*model) []pane
	reload   func(*model) tea.Cmd
	enter    func(*model) tea.Cmd
	ownsBack bool
}

// sectionSpecs lists the sections in nav order. It is a function rather than a
// package variable because the handlers it names reach back to it.
func sectionSpecs() [sectionCount]sectionSpec {
	return [sectionCount]sectionSpec{
		sectionDashboard: {
			label: "Dashboard", jump: "g",
			header: (*model).dashboardHeaderView, body: (*model).dashboardBody,
			reload: (*model).refreshDashboard,
		},
		sectionFiles: {
			label: "Files", jump: "f", ownsBack: true,
			keys: (*model).filesKeys, handle: (*model).handleFilesKey,
		},
		sectionBackends: {
			label: "Backends", jump: "b",
			keys: (*model).backendsKeys, handle: (*model).handleBackendsKey,
			header: (*model).backendsHeaderView, body: (*model).backendsBody,
			reload: func(m *model) tea.Cmd {
				m.backends.loading = !m.backends.list.loaded()
				return m.fetch(pollStatus)
			},
		},
		sectionBuckets: {
			label: "Buckets", jump: "v",
			keys:   fixedKeys(hintMove, hintSort, hintReverse, hintReload, hintNavBack),
			handle: (*model).handleBucketsKey,
			header: (*model).bucketsHeaderView, body: (*model).bucketsBody,
			reload: func(m *model) tea.Cmd {
				m.buckets.loading = !m.buckets.list.loaded()
				return m.fetch(pollBuckets)
			},
		},
		sectionReplication: {
			label: "Replication", jump: "p",
			header: (*model).replicationHeaderView, body: (*model).replicationBody,
			reload: func(m *model) tea.Cmd {
				m.replication.loading = m.replication.snap == nil
				return m.fetch(pollReplication)
			},
		},
		sectionWorkers: {
			label: "Workers", jump: "w",
			keys: fixedKeys(hintMove, keyHint{key: "R", desc: "run the matching Ops action", label: "run"},
				hintSort, hintReverse, hintReload, hintNavBack),
			handle: (*model).handleWorkersKey,
			header: (*model).workersHeaderView, body: (*model).workersBody,
			reload: func(m *model) tea.Cmd {
				m.workers.loading = !m.workers.list.loaded()
				return m.fetch(pollWorkers)
			},
		},
		sectionCleanup: {
			label: "Cleanup", jump: "u",
			keys: (*model).cleanupKeys, handle: (*model).handleCleanupKey,
			header: (*model).cleanupHeaderView, body: (*model).cleanupBody,
			reload: func(m *model) tea.Cmd {
				m.cleanup.loading = !m.cleanup.fetched
				return m.fetch(pollCleanup)
			},
		},
		sectionCache: {
			label: "Cache", jump: "c",
			header: (*model).cacheHeaderView, body: (*model).cacheBody,
			reload: func(m *model) tea.Cmd {
				m.cache.loading = m.cache.snap == nil
				return m.fetch(pollCache)
			},
		},
		sectionConfig: {
			label: "Config", jump: "n",
			keys:   fixedKeys(hintScroll, hintReload, hintNavBack),
			handle: (*model).handleConfigKey,
			header: (*model).configHeaderView, body: (*model).configBody,
			reload: func(m *model) tea.Cmd {
				m.config.loading = m.config.resp == nil
				return m.loadConfig()
			},
		},
		sectionLogs: {
			label: "Logs", jump: "l", ownsBack: true,
			keys: fixedKeys(hintScroll,
				keyHint{key: "/", desc: "filter by text in the component or message", label: "filter"},
				keyHint{key: "L", desc: "cycle the minimum level", label: "level"},
				keyHint{key: "F", desc: "follow new entries as they arrive", label: "follow"},
				keyHint{key: "esc", desc: "clear the filter, then back to the nav"}, hintReload),
			handle: (*model).handleLogsKey,
			header: (*model).logsHeaderView, body: (*model).logsBody,
			enter: (*model).enterLogs,
		},
		sectionOps: {
			label: "Ops", jump: "o", ownsBack: true,
			keys: (*model).opsKeys, handle: (*model).handleOpsKey,
			header: (*model).opsHeaderView,
			body:   func(m *model) []pane { return []pane{textPane(m.opsMenuView())} },
			enter:  (*model).enterOps,
		},
	}
}

// spec is the active section's entry.
func (m *model) spec() sectionSpec {
	return sectionSpecs()[m.section]
}

// fixedKeys is the keymap of a pane whose keys never change.
func fixedKeys(hints ...keyHint) func(*model) []keyHint {
	return func(*model) []keyHint { return hints }
}

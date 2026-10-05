// -------------------------------------------------------------------------------
// TUI - Keymap and Help
//
// Author: Alex Freidah
//
// Every key the TUI answers, declared once per pane. The footer shows the few
// keys that carry a short label, followed by "? help"; pressing "?" replaces
// the pane with its full keymap and the global keys, and any key closes it.
// Declaring the keys here, rather than writing a hint string in each footer,
// is what keeps the footer and the help from disagreeing.
// -------------------------------------------------------------------------------

package tui

import (
	"fmt"
	"strings"
)

// keyHint is one key and what it does. label is the short form shown in the
// pane's footer; a key without one is listed only in the help.
type keyHint struct {
	key   string
	desc  string
	label string
}

// sectionKey is a single-letter jump to a section.
type sectionKey struct {
	key string
	sec section
}

// sectionKeys lists the section jumps in nav order. The key handler and the
// help overlay both read it.
var sectionKeys = []sectionKey{
	{"g", sectionDashboard},
	{"f", sectionFiles},
	{"b", sectionBackends},
	{"v", sectionBuckets},
	{"p", sectionReplication},
	{"w", sectionWorkers},
	{"u", sectionCleanup},
	{"c", sectionCache},
	{"l", sectionLogs},
	{"o", sectionOps},
}

// keyUpDown names the arrow keys in a hint.
const keyUpDown = "up/down"

// Hints shared by several panes.
var (
	hintMove    = keyHint{key: keyUpDown, desc: "move the selection"}
	hintScroll  = keyHint{key: keyUpDown, desc: "scroll"}
	hintReload  = keyHint{key: "r", desc: "reload now", label: "reload"}
	hintNavBack = keyHint{key: "esc", desc: "back to the nav"}
	hintSort    = keyHint{key: "s", desc: "sort by the next column, then back to the server's order", label: "sort"}
	hintReverse = keyHint{key: "S", desc: "reverse the sort"}
)

// paneKeys returns the active pane's keys. Keys that only apply in some
// states, such as cancelling a drain or requeueing from the dead-letter
// listing, are listed only while they apply.
func (m *model) paneKeys() []keyHint {
	switch m.section {
	case sectionFiles:
		return m.filesKeys()
	case sectionBackends:
		return m.backendsKeys()
	case sectionCleanup:
		return m.cleanupKeys()
	case sectionOps:
		return m.opsKeys()
	case sectionLogs:
		return []keyHint{hintScroll,
			{key: "/", desc: "filter by text in the component or message", label: "filter"},
			{key: "L", desc: "cycle the minimum level", label: "level"},
			{key: "esc", desc: "clear the filter, then back to the nav"}, hintReload}
	case sectionBuckets, sectionWorkers:
		return []keyHint{hintMove, hintSort, hintReverse, hintReload, hintNavBack}
	default:
		return []keyHint{hintReload, hintNavBack}
	}
}

// filesKeys is the listing's keymap, or the inspector's while it is open.
func (m *model) filesKeys() []keyHint {
	if m.mode == modeInspect {
		return []keyHint{hintMove, {key: "esc", desc: "back to the listing", label: "back"},
			{key: "S", desc: "verify every copy now", label: "scrub"}, hintReload}
	}
	return []keyHint{
		hintMove,
		{key: "enter", desc: "open a prefix or inspect an object", label: "open"},
		{key: "backspace", desc: "up one prefix"},
		{key: "/", desc: "filter the listing", label: "filter"},
		{key: "esc", desc: "clear the filter"},
		{key: "s", desc: "sort by name or size", label: "sort"},
		{key: "D", desc: "download the object, or everything under a prefix", label: "download"},
		{key: "U", desc: "upload a file here", label: "upload"},
		{key: "X", desc: "delete the object or prefix", label: "delete"},
		hintReload,
	}
}

// backendsKeys is the backends keymap. Cancel is listed only while the pane
// is following a drain, so it cannot read as available with nothing to stop.
func (m *model) backendsKeys() []keyHint {
	keys := []keyHint{hintMove,
		{key: "enter", desc: "the backend's action menu", label: "actions"},
		{key: "d", desc: "drain the backend", label: "drain"},
		{key: "R", desc: "reconcile metadata against the backend", label: "reconcile"},
		{key: "Q", desc: "requeue the backend's dead-lettered cleanups"},
	}
	if m.backends.drain.backend != "" {
		keys = append(keys, keyHint{key: "x", desc: "cancel the drain", label: "cancel drain"})
	}
	return append(keys, hintSort, hintReverse, hintReload, hintNavBack)
}

// cleanupKeys is the cleanup keymap. Requeue is listed only on the
// dead-letter listing it applies to.
func (m *model) cleanupKeys() []keyHint {
	keys := []keyHint{hintMove}
	if m.cleanupOnDLQ() {
		keys = append(keys, keyHint{key: "t", desc: "show the pending queue", label: "pending"},
			keyHint{key: "R", desc: "requeue the backend's dead-lettered rows", label: "requeue backend"})
	} else {
		keys = append(keys, keyHint{key: "t", desc: "show the dead-letter listing", label: "dead-letter"})
	}
	return append(keys, hintSort, hintReverse, hintReload, hintNavBack)
}

// opsKeys is the ops keymap for the menu, or for the output of an action.
func (m *model) opsKeys() []keyHint {
	switch {
	case m.ops.showOut && m.ops.running:
		return []keyHint{{key: keyUpDown, desc: "scroll the output", label: "scroll"}}
	case m.ops.showOut:
		return []keyHint{hintScroll, {key: "esc", desc: "back to the menu", label: "back"}}
	default:
		return []keyHint{hintMove, {key: "enter", desc: "run the action", label: "run"}, hintNavBack}
	}
}

// globalKeys is what every pane answers, in the help overlay's order.
func globalKeys() []keyHint {
	labels := make(map[section]string, len(sectionKeys))
	for _, e := range navEntries() {
		labels[e.sec] = e.label
	}
	keys := []keyHint{{key: "tab", desc: "move focus between the nav and the pane"}}
	for _, s := range sectionKeys {
		keys = append(keys, keyHint{key: s.key, desc: labels[s.sec]})
	}
	return append(keys, keyHint{key: "?", desc: "this help"}, keyHint{key: "q", desc: "quit"})
}

// hintLine is the footer's key hints: the pane's labelled keys, then help and
// quit.
func (m *model) hintLine() string {
	var parts []string
	for _, k := range m.paneKeys() {
		if k.label != "" {
			parts = append(parts, k.key+" "+k.label)
		}
	}
	parts = append(parts, "? help", "q quit")
	return strings.Join(parts, " - ")
}

// hintFooter is the footer every pane shows: a prompt or the last action's
// result when there is one, otherwise the pane's key hints.
func (m *model) hintFooter() string {
	return m.footer(m.hintLine())
}

// sectionLabel is the nav label of the active section.
func (m *model) sectionLabel() string {
	for _, e := range navEntries() {
		if e.sec == m.section {
			return e.label
		}
	}
	return ""
}

// helpPaneView replaces the active pane with its keymap and the global keys.
func (m *model) helpPaneView() string {
	label := m.sectionLabel()
	header := m.contentTitleStyle().Width(m.contentWidth()).Render("keys   " + label)
	body := keyBlock(label, m.paneKeys()) + "\n\n" + keyBlock("everywhere", globalKeys())
	return m.frame(header, m.footer("any key closes this help"), textPane(body))
}

// keyBlock renders a titled, aligned list of keys.
func keyBlock(title string, keys []keyHint) string {
	width := 0
	for _, k := range keys {
		width = max(width, len(k.key))
	}
	lines := []string{colHeaderStyle.Render(title)}
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("  %-*s  %s", width, k.key, k.desc))
	}
	return strings.Join(lines, "\n")
}

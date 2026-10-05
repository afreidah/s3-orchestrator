// -------------------------------------------------------------------------------
// TUI - Object Inspector
//
// Author: Alex Freidah
//
// Detail pane for a single object key. Fetches every backend copy from the
// admin object-locations endpoint and renders the per-copy ledger (backend,
// size, age, encryption, content hash) so an operator can see exactly where an
// object lives and how its replicas compare. Read-only, like the browser.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// viewMode selects which pane the model is showing.
type viewMode int

const (
	modeBrowse  viewMode = iota // the prefix listing
	modeInspect                 // one object's copies
)

// inspector holds the state of the object-detail pane.
type inspector struct {
	key       string                    // the object key under inspection
	locations []adminapi.ObjectLocation // per-backend copies, in load order
	tags      []adminapi.ObjectTag      // the object's tag set, ordered by key
	table     table.Model               // scrolling table over the copies
	loading   bool                      // a locations fetch is in flight
	scrubbing bool                      // a targeted scrub is in flight
	err       error                     // last fetch error, if any
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// locationsLoadedMsg carries a successfully loaded location ledger.
type locationsLoadedMsg struct {
	resp *adminapi.ObjectLocationsResponse
}

// locationsErrMsg carries a failed locations fetch.
type locationsErrMsg struct{ err error }

// loadLocations returns a command that fetches every copy of key off the main
// loop, delivering the result back as a locationsLoadedMsg or locationsErrMsg.
func (m *model) loadLocations(key string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.GetObjectLocations(context.Background(), key)
		if err != nil {
			return locationsErrMsg{err}
		}
		return locationsLoadedMsg{resp}
	}
}

// tagsLoadedMsg carries an object's tag set, or the error that prevented
// reading it.
type tagsLoadedMsg struct {
	tags []adminapi.ObjectTag
	err  error
}

// loadTags returns a command that fetches key's tag set off the main loop.
//
// Separate from the locations load so a tag read that fails leaves the copy
// ledger intact: the ledger is what the pane exists for, and tags are context
// beside it.
func (m *model) loadTags(key string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.GetObjectTags(context.Background(), key)
		if err != nil {
			return tagsLoadedMsg{err: err}
		}
		return tagsLoadedMsg{tags: resp.Tags}
	}
}

// scrubKeyMsg carries the outcome of a targeted scrub.
type scrubKeyMsg struct {
	resp *adminapi.ScrubKeyResponse
	err  error
}

// scrubKey returns a command that verifies every copy of key off the main loop.
func (m *model) scrubKey(key string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.ScrubKey(context.Background(), key)
		return scrubKeyMsg{resp: resp, err: err}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// openInspector switches to the inspector pane for key and kicks off its load.
func (m *model) openInspector(key string) (tea.Model, tea.Cmd) {
	m.mode = modeInspect
	m.insp = inspector{key: key, loading: true, table: newTable(inspectorColumns)}
	return m, tea.Batch(m.loadLocations(key), m.loadTags(key))
}

// applyLocations folds a loaded ledger into the inspector state.
func (m *model) applyLocations(resp *adminapi.ObjectLocationsResponse) {
	m.insp.locations = resp.Locations
	m.insp.table.SetRows(rowsFromLocations(resp.Locations))
	m.insp.table.SetCursor(0)
	m.insp.loading = false
	m.insp.err = nil
}

// applyTags folds a loaded tag set into the inspector state.
//
// A failed read leaves the set empty and does not set insp.err: the pane's
// error line belongs to the copy ledger, and blanking that because tags could
// not be read would hide what the operator opened the pane to see.
func (m *model) applyTags(msg tagsLoadedMsg) {
	if msg.err != nil {
		m.insp.tags = nil
		return
	}
	m.insp.tags = msg.tags
}

// handleInspectKey applies inspector-level keys (quit, back, reload) and
// delegates cursor movement to the table.
func (m *model) handleInspectKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "backspace", "left", "h":
		m.mode = modeBrowse
		return m, nil
	case "r":
		m.insp.loading = true
		cmd := m.loadLocations(m.insp.key)
		return m, cmd
	case "S":
		return m.armScrubKey()
	}

	var cmd tea.Cmd
	m.insp.table, cmd = m.insp.table.Update(key)
	return m, cmd
}

// armScrubKey confirms verifying every copy of the inspected object. The prompt
// spells out what a failure costs, because this is not a read-only check: a copy
// whose bytes do not match its hash is discarded here exactly as the sweep would
// discard it.
func (m *model) armScrubKey() (tea.Model, tea.Cmd) {
	if m.insp.key == "" || m.insp.scrubbing {
		return m, nil
	}
	return m.startAction(adminAction{
		confirm: "Verify every copy of " + m.insp.key + " now? A copy that fails is discarded and rebuilt.",
		before:  func(m *model) { m.insp.scrubbing = true },
		run:     m.scrubKey(m.insp.key),
	})
}

// applyScrubKey reports the per-copy verdict and reloads the ledger, since a
// discarded copy is gone from it and a verified one carries a fresh timestamp.
func (m *model) applyScrubKey(msg scrubKeyMsg) (tea.Model, tea.Cmd) {
	m.insp.scrubbing = false
	if msg.err != nil {
		m.report(false, "scrub failed: "+msg.err.Error())
		return m, nil
	}

	ok, summary := scrubSummary(msg.resp.Copies)
	m.report(ok, summary)
	m.insp.loading = true
	cmd := m.loadLocations(m.insp.key)
	return m, cmd
}

// scrubSummary renders the verdicts as one footer line, reporting whether every
// copy passed. Copies that passed are counted; ones that did not are named,
// because which backend holds the bad copy is the actionable half and a count
// would bury it.
func scrubSummary(copies []adminapi.CopyScrubResult) (bool, string) {
	verified := 0
	var bad []string
	for _, c := range copies {
		if c.Outcome == adminapi.CopyVerified {
			verified++
			continue
		}
		bad = append(bad, c.Backend+" "+c.Outcome)
	}

	if len(bad) == 0 {
		return true, "scrub: " + countOf(verified, "copy", "copies") + " verified"
	}
	return false, fmt.Sprintf("scrub: %d of %d verified - %s",
		verified, len(copies), strings.Join(bad, ", "))
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// inspectorColumns declares the copy table's columns. The backend is capped so
// short names don't sprawl on a wide terminal. A narrow terminal keeps what
// compares one copy against another (size, hash, verification) and drops the
// key ID and creation time first.
var inspectorColumns = []columnSpec{
	{title: "BACKEND", min: 8, max: 24, priority: 9},
	{title: "SIZE", min: 12, max: 12, priority: 8},
	{title: "LOGICAL", min: 12, max: 12, priority: 3},
	{title: "COMP", min: 6, max: 6, priority: 4},
	{title: "CREATED", min: 14, max: 14, priority: 2},
	{title: "ENC", min: 5, max: 5, priority: 5},
	{title: "KEY ID", min: 12, max: 12, priority: 1},
	{title: "HASH", min: 18, max: 18, priority: 7},
	{title: "VERIFIED", min: 10, max: 10, priority: 6},
}

// rowsFromLocations builds inspector rows in the same order as the ledger so
// the table cursor indexes straight into the copies.
func rowsFromLocations(locations []adminapi.ObjectLocation) []table.Row {
	rows := make([]table.Row, 0, len(locations))
	for i := range locations {
		l := &locations[i]
		rows = append(rows, table.Row{
			l.Backend,
			humanize.Bytes(l.SizeBytes),
			logicalSize(l),
			compressionMark(l),
			relativeAge(l.CreatedAt),
			yesNo(l.Encrypted),
			truncate(l.KeyID, 10),
			truncate(l.ContentHash, 16),
			verifiedAge(l.LastScrubbedAt),
		})
	}
	return rows
}

// logicalSize renders the size the client wrote, which differs from the stored
// size only for an encoded copy. A dash for the rest says "same as SIZE"
// without repeating the number and inviting the reader to compare them.
func logicalSize(l *adminapi.ObjectLocation) string {
	if l.CompressionAlgorithm == "" {
		return "-"
	}
	return humanize.Bytes(l.LogicalSize)
}

// compressionMark renders how a copy is encoded, or a dash when it is stored
// verbatim.
func compressionMark(l *adminapi.ObjectLocation) string {
	if l.CompressionAlgorithm == "" {
		return "-"
	}
	return l.CompressionAlgorithm
}

// inspectView composes the inspector's full-screen layout.
func (m *model) inspectView() string {
	return m.frame(m.inspectHeaderView(), m.hintFooter(), m.inspectBody()...)
}

// inspectHeaderView renders the title bar with the key and copy count.
func (m *model) inspectHeaderView() string {
	title := fmt.Sprintf("inspect   %s   (%d copies)", m.insp.key, len(m.insp.locations))
	if m.insp.scrubbing {
		title += "   verifying..."
	}
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title)
}

// inspectBody renders the current content: an error, the loading indicator, an
// empty notice, or the tag line above the copy table.
func (m *model) inspectBody() []pane {
	return m.paneBody(m.insp.err, "", m.insp.loading, func() []pane {
		if len(m.insp.locations) == 0 {
			return []pane{textPane(pathStyle.Render("(no copies found)"))}
		}
		return []pane{textPane(m.inspectTagsView()), m.tablePane(&m.insp.table, inspectorColumns)}
	})
}

// inspectTagsView renders the object's tag set as one line above the copy
// table. Tags belong to the object rather than to any copy, so they sit
// outside the per-backend table rather than as a column repeated down it.
func (m *model) inspectTagsView() string {
	label := tagLabelStyle.Render("tags:")
	if len(m.insp.tags) == 0 {
		return label + " " + pathStyle.Render("(none)")
	}
	pairs := make([]string, len(m.insp.tags))
	for i, t := range m.insp.tags {
		pairs[i] = t.Key + "=" + t.Value
	}
	return label + " " + tagValueStyle.Render(strings.Join(pairs, "  "))
}

// -------------------------------------------------------------------------
// FORMATTING HELPERS
// -------------------------------------------------------------------------

// relativeAge renders how long ago t was in a compact, coarse form. A zero time
// renders as a dash.
func relativeAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}

// verifiedAge renders when a copy was last checked against its stored hash.
// "never" is its own answer, not a missing value: a recorded hash only says
// what the bytes were meant to be, and until something reads them back nobody
// knows whether that copy is still intact.
func verifiedAge(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return relativeAge(*t)
}

// yesNo renders a boolean as a compact yes/no.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// truncate shortens s to n runes, marking the cut with a trailing tilde.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "~"
}

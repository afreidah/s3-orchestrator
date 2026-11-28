// -------------------------------------------------------------------------------
// TUI - Dashboard
//
// Author: Alex Freidah
//
// The section the TUI opens on: a summary of the fleet, so an operator can see
// whether anything is wrong without visiting every pane. The summary at the top
// shows the fleet's storage against its limits, the limit nearest full, the
// replication and verification backlogs, encryption and compression coverage,
// usage for the period, the cleanup queues and the session's newest action
// results. Below it, each backend gets a block of its own: a rule naming it
// with its health and usage cycle, over its storage and each usage dimension
// against its limit, if it has one. The summary stays put and the blocks
// scroll, so a fleet taller than the terminal is read rather than cut off.
//
// Every figure here is fleet-wide, so the dashboard reads the same whichever
// instance answers. All of it comes from the status, replication and cleanup
// snapshots the poller keeps fresh on every pane, so the dashboard makes no
// requests of its own. Reached with "g"; up and down scroll the backends, "r"
// reloads.
// -------------------------------------------------------------------------------

package tui

import (
	"fmt"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// Layout of a meter block: the widest a row's name renders before it is
// truncated, and the narrowest and widest a usage bar is drawn.
const (
	dashNameMax = 24
	dashBarMin  = 10
	dashBarMax  = 40
)

// summaryLabelWidth aligns the labels of the label/value blocks the dashboard,
// the cache pane and the backend detail draw.
const summaryLabelWidth = 14

// dashActionsShown is how many of the session's newest action results the
// dashboard lists.
const dashActionsShown = 3

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// dashboardView holds the dashboard's scroll position over the backend blocks.
type dashboardView struct {
	vp viewport.Model
}

// meterRow is one line of a meter block: a ceiling drawn under the row's name,
// followed by its markers.
type meterRow struct {
	output.Ceiling
	name    string
	markers string
}

// meterColumns holds the widths that line rows up: the name, the bar, and the
// used and limit figures.
type meterColumns struct {
	name  int
	bar   int
	used  int
	limit int
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// refreshDashboard requests every snapshot the dashboard reads. Requests the
// poller already has in flight are skipped.
func (m *model) refreshDashboard() tea.Cmd {
	return tea.Batch(m.fetch(pollStatus), m.fetch(pollReplication), m.fetch(pollCleanup))
}

// handleDashboardKey scrolls the backend blocks.
func (m *model) handleDashboardKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.dashboard.vp, cmd = m.dashboard.vp.Update(key)
	return m, cmd
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// dashboardHeaderView renders the title bar with the backend count.
func (m *model) dashboardHeaderView() string {
	title := fmt.Sprintf("dashboard   %d backends", len(m.backends.list.received))
	return m.titleBar(title)
}

// dashboardBody renders the summary, the session's newest action results once
// anything has been run, then the backend blocks in a viewport that takes the
// rows left over. It waits for the first status snapshot, since every figure
// but replication comes from it; a status error is shown in its place.
func (m *model) dashboardBody() []pane {
	state := m.backends.loadState
	state.loading = m.dbHealthy == nil && state.err == nil
	return m.paneBody(&state, func() []pane {
		cols := m.dashboardColumns()
		body := []pane{textPane(m.dashboardSummary(cols))}
		if len(m.actionLog) > 0 {
			body = append(body, textPane(""), textPane(m.recentActions()))
		}
		m.dashboard.vp.SetContent(m.dashboardBackends(cols))
		return append(body, textPane(""), m.viewportPane(&m.dashboard.vp))
	})
}

// recentActions lists the session's newest action results under a heading,
// the newest last.
func (m *model) recentActions() string {
	entries := m.actionLog[max(len(m.actionLog)-dashActionsShown, 0):]
	lines := make([]string, 0, len(entries)+1)
	lines = append(lines, colHeaderStyle.Render("recent actions"))
	for _, e := range entries {
		style := statusOKStyle
		if !e.ok {
			style = statusErrStyle
		}
		lines = append(lines, pathStyle.Render(e.at.Format("15:04:05"))+"  "+style.Render(e.text))
	}
	return strings.Join(lines, "\n")
}

// dashboardColumns sizes the meter columns over the fleet's storage and every
// backend's rows, so the summary's storage bar and every bar below it start
// in one column and their figures line up.
func (m *model) dashboardColumns() meterColumns {
	rows := []meterRow{{Ceiling: m.fleetStorage()}}
	for i := range m.backends.list.received {
		rows = append(rows, backendMeterRows(&m.backends.list.received[i], nil)...)
	}
	return m.meterColumns(rows)
}

// dashboardBackends renders a block per backend, laid out to cols: a rule
// naming the backend with its health and flags, and its usage cycle when it
// has usage limits, over its storage row and a row per usage dimension. A
// blank line separates one block from the next.
func (m *model) dashboardBackends(cols meterColumns) string {
	if len(m.backends.list.received) == 0 {
		return pathStyle.Render("(no backends)")
	}
	width := m.contentWidth()
	var lines []string
	for i := range m.backends.list.received {
		b := &m.backends.list.received[i]
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, ruleLine(colHeaderStyle.Render(b.Name)+"  "+backendMarkers(b), backendCycle(b), width))
		rows := backendMeterRows(b, nil)
		for j := range rows {
			lines = append(lines, meterLine(&rows[j], cols))
		}
	}
	return strings.Join(lines, "\n")
}

// backendCycle renders a backend's usage cycle, or nothing for one with no
// usage limits, whose cycle measures nothing.
func backendCycle(b *adminapi.BackendStatus) string {
	if len(b.UsageLimits) == 0 {
		return ""
	}
	return pathStyle.Render(output.Cycle(b.UsageCycle))
}

// backendMarkers renders a backend's breaker health - naming the refused calls
// when only some are refused - and, when it carries any, its state flags.
func backendMarkers(b *adminapi.BackendStatus) string {
	var health string
	switch core.CallsNamed(b.RefusedCalls).Health() {
	case core.HealthDown:
		health = statusErrStyle.Render("DOWN")
	case core.HealthDegraded:
		health = logLevelWarn.Render("DEGRADED " + output.NameList(b.RefusedCalls))
	default:
		health = statusOKStyle.Render(core.HealthHealthy)
	}
	if len(b.Flags) == 0 {
		return health
	}
	return health + "  " + logLevelWarn.Render(output.NameList(b.Flags))
}

// -------------------------------------------------------------------------
// METERS
// -------------------------------------------------------------------------

// backendMeterRows returns a backend's storage row and a row per usage
// dimension, each named for what it measures. marker, when set, renders what
// follows a usage row's figures.
func backendMeterRows(b *adminapi.BackendStatus, marker func(adminapi.UsageMeter) string) []meterRow {
	storage := output.StorageCeiling(b)
	rows := []meterRow{{Ceiling: storage, name: storage.Label}}
	for _, u := range b.UsageLimits {
		c := output.UsageCeiling(u)
		row := meterRow{Ceiling: c, name: c.Label}
		if marker != nil {
			row.markers = marker(u)
		}
		rows = append(rows, row)
	}
	return rows
}

// meterColumns sizes the columns for rows. The name column is at least as wide
// as a label/value block's labels, so a bar starts where their values do.
// Every bar has the same width: what the content area leaves after the name
// and the widest figures, so the figures line up and no line wraps while there
// is room for a bar of the minimum width.
func (m *model) meterColumns(rows []meterRow) meterColumns {
	cols := meterColumns{name: summaryLabelWidth}
	for _, r := range rows {
		cols.name = max(cols.name, len(r.name))
		cols.used = max(cols.used, len(r.Format(r.Used)))
		if r.Limit > 0 {
			cols.limit = max(cols.limit, len(r.Format(r.Limit)))
		}
	}
	cols.name = min(cols.name, dashNameMax)
	figuresW := 0
	for i := range rows {
		figuresW = max(figuresW, lipgloss.Width(meterFigures(&rows[i], cols)))
	}
	// The space after the name, the bar's two brackets, and two spaces before
	// the figures.
	cols.bar = min(max(m.contentWidth()-cols.name-figuresW-5, dashBarMin), dashBarMax)
	return cols
}

// meterBlock renders rows under a heading, laid out to cols.
func meterBlock(heading string, cols meterColumns, rows []meterRow) string {
	lines := []string{colHeaderStyle.Render(heading)}
	for i := range rows {
		lines = append(lines, meterLine(&rows[i], cols))
	}
	return strings.Join(lines, "\n")
}

// meterLine renders a row's name followed by its meter.
func meterLine(r *meterRow, cols meterColumns) string {
	return fmt.Sprintf("%-*s ", cols.name, truncate(r.name, cols.name)) + meterValue(r, cols)
}

// meterValue renders a row's usage bar and the figures behind it. A row with
// no limit has nothing to fill a bar against, so the bar's place says it is
// unlimited.
func meterValue(r *meterRow, cols meterColumns) string {
	bar := pathStyle.Render(fmt.Sprintf("%-*s", cols.bar+2, output.NoLimit))
	if r.Limit > 0 {
		bar = usageBar(r.Percent(), cols.bar)
	}
	return bar + "  " + meterFigures(r, cols)
}

// meterFigures renders what a row holds against its limit, then its markers.
// Each figure is padded to its column, so the percentages and markers line up
// down the block; a row with no limit leaves those columns blank.
func meterFigures(r *meterRow, cols meterColumns) string {
	figures := fmt.Sprintf("%*s", cols.used, r.Format(r.Used))
	if r.Limit > 0 {
		figures += fmt.Sprintf(" / %-*s  ", cols.limit, r.Format(r.Limit)) + percentText(r.Percent(), 4)
	} else if cols.limit > 0 {
		figures += strings.Repeat(" ", 3+cols.limit+2+4)
	}
	if r.markers != "" {
		figures += "  " + r.markers
	}
	return figures
}

// usageBar draws pct as a bracketed bar of width cells, filled with "#" and
// coloured like the percentage it stands for. A backend past its limit fills
// the bar rather than overrunning it.
func usageBar(pct, width int) string {
	filled := min(max(pct*width/100, 0), width)
	return "[" + usageStyle(pct).Render(strings.Repeat("#", filled)) + strings.Repeat("-", width-filled) + "]"
}

// -------------------------------------------------------------------------
// SUMMARY
// -------------------------------------------------------------------------

// dashboardSummary renders the fleet's storage, backlogs and coverage as an
// aligned label/value block, laid out to cols so the storage bar starts in the
// column the backend blocks' bars do.
func (m *model) dashboardSummary(cols meterColumns) string {
	iv := m.backends.integrity
	fleet := meterRow{Ceiling: m.fleetStorage()}
	return fieldBlock(cols.name,
		field{"fleet storage", meterValue(&fleet, cols)},
		field{"nearest limit", m.dashboardNearestLimit()},
		field{"database", m.dbText()},
		field{"replication", m.dashboardReplication()},
		field{"verified", integrityHeadline(iv) + deferredSuffix(iv.DeferredCopies)},
		field{"encryption", dashboardEncryption(iv.PlaintextCopies)},
		field{"compression", m.dashboardCompression()},
		field{"usage", m.dashboardUsage()},
		field{"cleanup", m.dashboardCleanup()},
	)
}

// dashboardNearestLimit renders the limit across the fleet that is nearest
// full: its backend and name, how full it is, its figures and, for a usage
// limit, how long until its cycle resets.
func (m *model) dashboardNearestLimit() string {
	n, ok := output.NearestLimit(m.backends.list.received)
	if !ok {
		return pathStyle.Render(output.NoLimits)
	}
	text := fmt.Sprintf("%s %s   %s   %s", n.Backend, n.Label, percentText(n.Percent(), 0), n.Figures())
	if reset := n.ResetText(); reset != "" {
		text += "   " + pathStyle.Render(reset)
	}
	return text
}

// dbText renders the metadata database's health from the last status fetch;
// before one lands it reads as unavailable.
func (m *model) dbText() string {
	if m.dbHealthy != nil && *m.dbHealthy {
		return statusOKStyle.Render("healthy")
	}
	return statusErrStyle.Render("UNAVAILABLE")
}

// dashboardReplication renders the deployment's factor, how many virtual
// buckets set their own, and the replication backlog, or says replication is
// off. A dash means the snapshot has not arrived yet.
func (m *model) dashboardReplication() string {
	s := m.replication.snap
	switch {
	case s == nil:
		return pathStyle.Render("-")
	case !replicating(s):
		return pathStyle.Render("disabled")
	}
	text := fmt.Sprintf("factor %d", s.Factor)
	if n := len(s.BucketFactors); n > 0 {
		text += "   " + pathStyle.Render(ownFactorsText(n))
	}
	return text + fmt.Sprintf("   %s under   %s over",
		backlogStyle(s.UnderReplicated).Render(humanize.Comma(s.UnderReplicated)),
		backlogStyle(s.OverReplicated).Render(humanize.Comma(s.OverReplicated)))
}

// dashboardEncryption renders how many copies are still stored in plaintext.
func dashboardEncryption(plaintext int64) string {
	if plaintext <= 0 {
		return statusOKStyle.Render("every copy encrypted")
	}
	return statusErrStyle.Render(humanize.Comma(plaintext)) + " plaintext copies"
}

// dashboardCompression renders what compression is saving across the fleet.
func (m *model) dashboardCompression() string {
	saved := m.fleetCompressionSaved()
	if saved <= 0 {
		return pathStyle.Render("nothing stored compressed")
	}
	return statusOKStyle.Render(humanize.Bytes(saved)) + " saved"
}

// dashboardUsage renders the fleet's request and transfer totals, each backend
// counted over its own current cycle.
func (m *model) dashboardUsage() string {
	var requests, ingress, egress int64
	for i := range m.backends.list.received {
		b := &m.backends.list.received[i]
		requests += b.APIRequests
		ingress += b.IngressBytes
		egress += b.EgressBytes
	}
	return fmt.Sprintf("%s requests   %s in   %s out   %s",
		humanize.Comma(requests), humanize.Bytes(ingress), humanize.Bytes(egress), pathStyle.Render("current cycles"))
}

// dashboardCleanup renders the depths of the cleanup queue and its dead-letter
// table. Dead-lettered rows need an operator, so a non-zero count stands out.
// A dash means the snapshot has not arrived yet.
func (m *model) dashboardCleanup() string {
	c := m.cleanup
	if !c.fetched {
		return pathStyle.Render("-")
	}
	dlq := statusOKStyle.Render("0")
	if c.dlqDepth > 0 {
		dlq = statusErrStyle.Render(humanize.Comma(c.dlqDepth))
	}
	return fmt.Sprintf("%s pending   %s dead-lettered", humanize.Comma(c.queueDepth), dlq)
}

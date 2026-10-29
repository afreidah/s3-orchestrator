// -------------------------------------------------------------------------------
// TUI - Dashboard
//
// Author: Alex Freidah
//
// The section the TUI opens on: a summary of the fleet, so an operator can see
// whether anything is wrong without visiting every pane. It shows capacity per
// backend and in total, the limit nearest full, the replication and
// verification backlogs, encryption and compression coverage, usage for the
// period, the cleanup queues, and then every backend's usage in each dimension
// against its limit, if it has one.
//
// Every figure here is fleet-wide, so the dashboard reads the same whichever
// instance answers. All of it comes from the status, replication and cleanup
// snapshots the poller keeps fresh on every pane, so the dashboard makes no
// requests of its own. Reached with "g"; "r" reloads.
// -------------------------------------------------------------------------------

package tui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Layout of the capacity block: the widest a backend name renders before it is
// truncated, and the narrowest and widest a usage bar is drawn.
const (
	dashNameMax = 24
	dashBarMin  = 10
	dashBarMax  = 40
)

// summaryLabelWidth aligns the labels of the label/value blocks the dashboard,
// the cache pane and the backend detail draw.
const summaryLabelWidth = 14

// dashFleetLabel names the capacity row that totals every backend.
const dashFleetLabel = "fleet"

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// refreshDashboard requests every snapshot the dashboard reads. Requests the
// poller already has in flight are skipped.
func (m *model) refreshDashboard() tea.Cmd {
	return tea.Batch(m.fetch(pollStatus), m.fetch(pollReplication), m.fetch(pollCleanup))
}

// handleDashboardKey applies dashboard keys (back, reload). The dashboard is a
// summary, so there is nothing to select.
func (m *model) handleDashboardKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "left", "h":
		return m.navBack()
	case "r":
		cmd := m.refreshDashboard()
		return m, cmd
	}
	return m, nil
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// dashboardPaneView composes the dashboard's full-screen layout.
func (m *model) dashboardPaneView() string {
	return m.frame(m.dashboardHeaderView(), m.hintFooter(), m.dashboardBody()...)
}

// dashboardHeaderView renders the title bar with the backend count.
func (m *model) dashboardHeaderView() string {
	title := fmt.Sprintf("dashboard   %d backends", len(m.backends.list.received))
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title)
}

// dashboardBody renders the capacity block, the summary block, the session's
// action log once anything has been run, then the usage block. Usage goes last
// because it lists every backend's every dimension, so on a short terminal it
// is what gets cut off. It waits for the first status snapshot, since every
// figure but replication comes from it; a status error is shown in its place.
func (m *model) dashboardBody() []pane {
	loading := m.dbHealthy == nil && m.backends.err == nil
	return m.paneBody(m.backends.err, "", loading, func() []pane {
		capacity, usage := m.dashboardMeters()
		body := []pane{textPane(capacity), textPane(""), textPane(m.dashboardSummary())}
		if len(m.actionLog) > 0 {
			body = append(body, textPane(""), m.actionLogPane())
		}
		if usage != "" {
			body = append(body, textPane(""), textPane(usage))
		}
		return body
	})
}

// actionLogPane lists the results of the actions run this session, newest
// last, under a heading. It takes the rows the dashboard has spare and shows
// as many of the newest results as fit.
func (m *model) actionLogPane() pane {
	return pane{height: 2, grows: true, render: func(height int) string {
		entries := m.actionLog[max(len(m.actionLog)-(height-1), 0):]
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
	}}
}

// meterRow is one line of a usage block: a ceiling drawn under the row's name
// and dimension, followed by its markers. A backend's usage rows name it only
// on the first, capacity rows leave the dimension blank, and the fleet total
// has no markers.
type meterRow struct {
	output.Ceiling
	name    string
	label   string
	markers string
}

// dashboardMeters renders the capacity block and the usage block over one set
// of columns, so names, bars and figures line up down both. The usage block is
// empty when no backend has reported any usage.
func (m *model) dashboardMeters() (capacity, usage string) {
	storage := m.dashboardCapacityRows()
	backends := m.dashboardUsageRows()
	cols := m.meterColumns(append([][]meterRow{storage}, backends...)...)
	if len(backends) > 0 {
		usage = meterBlock("usage limits", cols, backends...)
	}
	return meterBlock("capacity", cols, storage), usage
}

// dashboardCapacityRows returns one storage row per backend, with its health
// and drain state, and one for the fleet.
func (m *model) dashboardCapacityRows() []meterRow {
	rows := make([]meterRow, 0, len(m.backends.list.received)+1)
	for i := range m.backends.list.received {
		b := &m.backends.list.received[i]
		rows = append(rows, meterRow{Ceiling: output.StorageCeiling(b), name: b.Name, markers: backendMarkers(b)})
	}
	return append(rows, meterRow{Ceiling: m.fleetStorage(), name: dashFleetLabel})
}

// dashboardUsageRows returns a group of rows per backend, one per usage
// dimension, limited or not, each marked with the backend's own cycle. Only a
// group's first row names the backend.
func (m *model) dashboardUsageRows() [][]meterRow {
	var groups [][]meterRow
	for i := range m.backends.list.received {
		b := &m.backends.list.received[i]
		if len(b.UsageLimits) == 0 {
			continue
		}
		cycle := pathStyle.Render(output.Cycle(b.UsageCycle))
		rows := make([]meterRow, len(b.UsageLimits))
		for j, u := range b.UsageLimits {
			c := output.UsageCeiling(u)
			rows[j] = meterRow{Ceiling: c, label: c.Label, markers: cycle}
		}
		rows[0].name = b.Name
		groups = append(groups, rows)
	}
	return groups
}

// meterColumns holds the widths that line rows up: the name, the dimension,
// the bar, and the used and limit figures.
type meterColumns struct {
	name  int
	label int
	bar   int
	used  int
	limit int
}

// prefix is the width the name and dimension columns take, each with the
// space after it. Rows with no dimension anywhere leave that column out.
func (c meterColumns) prefix() int {
	if c.label == 0 {
		return c.name + 1
	}
	return c.name + c.label + 2
}

// meterColumns sizes the columns for every row of the given groups. Every bar
// has the same width: what the content area leaves after the widest name,
// dimension and figures, so the figures line up and no line wraps while there
// is room for a bar of the minimum width.
func (m *model) meterColumns(groups ...[]meterRow) meterColumns {
	rows := slices.Concat(groups...)
	var cols meterColumns
	for _, r := range rows {
		cols.name = max(cols.name, len(r.name))
		cols.label = max(cols.label, len(r.label))
		cols.used = max(cols.used, len(r.Format(r.Used)))
		if r.Limit > 0 {
			cols.limit = max(cols.limit, len(r.Format(r.Limit)))
		}
	}
	cols.name = min(cols.name, dashNameMax)
	cols.label = min(cols.label, dashNameMax)
	figuresW := 0
	for i := range rows {
		figuresW = max(figuresW, lipgloss.Width(meterFigures(&rows[i], cols)))
	}
	// The bar's two brackets, and two spaces before the figures.
	cols.bar = min(max(m.contentWidth()-cols.prefix()-figuresW-4, dashBarMin), dashBarMax)
	return cols
}

// meterBlock renders groups of rows under a heading, laid out to cols, with a
// blank line between groups.
func meterBlock(heading string, cols meterColumns, groups ...[]meterRow) string {
	lines := []string{colHeaderStyle.Render(heading)}
	for i, rows := range groups {
		if i > 0 {
			lines = append(lines, "")
		}
		for j := range rows {
			lines = append(lines, meterLine(&rows[j], cols))
		}
	}
	return strings.Join(lines, "\n")
}

// meterLine renders a row's name and dimension, its usage bar and the figures
// behind it. A row with no limit has nothing to fill a bar against, so the
// bar's place says it is unlimited.
func meterLine(r *meterRow, cols meterColumns) string {
	line := fmt.Sprintf("%-*s ", cols.name, truncate(r.name, cols.name))
	if cols.label > 0 {
		line += fmt.Sprintf("%-*s ", cols.label, truncate(r.label, cols.label))
	}
	bar := pathStyle.Render(fmt.Sprintf("%-*s", cols.bar+2, output.NoLimit))
	if r.Limit > 0 {
		bar = usageBar(r.Percent(), cols.bar)
	}
	return line + bar + "  " + meterFigures(r, cols)
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

// dashboardSummary renders the fleet's backlogs and coverage as an aligned
// label/value block.
func (m *model) dashboardSummary() string {
	iv := m.backends.integrity
	return fieldBlock(summaryLabelWidth,
		field{"nearest limit", m.dashboardNearestLimit()},
		field{"database", m.dashboardDB()},
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

// dashboardDB renders the metadata database's health.
func (m *model) dashboardDB() string {
	if m.backends.dbHealthy {
		return statusOKStyle.Render("healthy")
	}
	return statusErrStyle.Render("UNAVAILABLE")
}

// dashboardReplication renders the replication backlog, or says replication is
// off. A dash means the snapshot has not arrived yet.
func (m *model) dashboardReplication() string {
	s := m.replication.snap
	switch {
	case s == nil:
		return pathStyle.Render("-")
	case s.Factor <= 1:
		return pathStyle.Render("disabled")
	}
	return fmt.Sprintf("factor %d   %s under   %s over", s.Factor,
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
	if !c.loaded {
		return pathStyle.Render("-")
	}
	dlq := statusOKStyle.Render("0")
	if c.dlqDepth > 0 {
		dlq = statusErrStyle.Render(humanize.Comma(c.dlqDepth))
	}
	return fmt.Sprintf("%s pending   %s dead-lettered", humanize.Comma(c.queueDepth), dlq)
}

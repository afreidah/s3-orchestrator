// -------------------------------------------------------------------------------
// TUI - Dashboard
//
// Author: Alex Freidah
//
// The section the TUI opens on: a summary of the fleet, so an operator can see
// whether anything is wrong without visiting every pane. It shows capacity per
// backend and in total, each backend's usage against its egress, ingress and
// request limits, the replication and verification backlogs, encryption and
// compression coverage, usage for the period, and the cleanup queues.
//
// Every figure here is fleet-wide, so the dashboard reads the same whichever
// instance answers. All of it comes from the status, replication and cleanup
// snapshots the poller keeps fresh on every pane, so the dashboard makes no
// requests of its own. Reached with "g"; "r" reloads.
// -------------------------------------------------------------------------------

package tui

import (
	"fmt"
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

// dashboardBody renders the capacity block, the usage-limits block when any
// backend has a limit, then the summary block, and the session's action log
// below them once anything has been run. It waits for
// the first status snapshot, since every figure but replication comes from
// it; a status error is shown in its place.
func (m *model) dashboardBody() []pane {
	loading := m.dbHealthy == nil && m.backends.err == nil
	return m.paneBody(m.backends.err, "", loading, func() []pane {
		body := []pane{textPane(m.dashboardCapacity()), textPane("")}
		if limits := m.dashboardUsageLimits(); limits != "" {
			body = append(body, textPane(limits), textPane(""))
		}
		body = append(body, textPane(m.dashboardSummary()))
		if len(m.actionLog) > 0 {
			body = append(body, textPane(""), m.actionLogPane())
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

// meterRow is one line of a usage block: what something has used against its
// limit, in the units format renders. The fleet total and the usage-limit rows
// have no markers.
type meterRow struct {
	name    string
	used    int64
	limit   int64
	format  func(int64) string
	markers string
}

// dashboardCapacity renders one storage bar per backend and one for the fleet,
// each followed by its used and limit bytes and, for backends, its health and
// drain state.
func (m *model) dashboardCapacity() string {
	rows := make([]meterRow, 0, len(m.backends.list.received)+1)
	for i := range m.backends.list.received {
		b := &m.backends.list.received[i]
		rows = append(rows, storageRow(b.Name, b, backendMarkers(b)))
	}
	used, limit := m.fleetStorage()
	rows = append(rows, meterRow{name: dashFleetLabel, used: used, limit: limit, format: humanize.Bytes})
	return m.meterBlock("capacity", rows)
}

// storageRow is the row of a usage block that draws a backend's stored bytes
// against its quota.
func storageRow(name string, b *adminapi.BackendStatus, markers string) meterRow {
	return meterRow{name: name, used: b.BytesUsed, limit: b.BytesLimit, format: humanize.Bytes, markers: markers}
}

// dashboardUsageLimits renders one bar per usage limit a backend enforces,
// each marked with the backend's own cycle. Unlimited dimensions never appear,
// and with no limits anywhere the block is left out.
func (m *model) dashboardUsageLimits() string {
	var rows []meterRow
	for i := range m.backends.list.received {
		b := &m.backends.list.received[i]
		cycle := pathStyle.Render(cycleText(b.UsageCycle))
		for _, u := range b.UsageLimits {
			rows = append(rows, usageMeterRow(b.Name+" "+output.MeterLabel(u.Kind, u.Budget), u, cycle))
		}
	}
	if len(rows) == 0 {
		return ""
	}
	return m.meterBlock("usage limits", rows)
}

// usageMeterRow is the row of a usage block that draws one limit a backend
// enforces, in the units its kind counts.
func usageMeterRow(name string, u adminapi.UsageMeter, markers string) meterRow {
	return meterRow{name: name, used: u.Used, limit: u.Limit, format: output.MeterFormat(u.Kind), markers: markers}
}

// cycleText names the usage cycle a backend's figures are counted in.
func cycleText(c adminapi.UsageCycle) string {
	return humanize.Cycle(c.Start, c.NextReset, c.Timezone)
}

// meterBlock renders rows under a heading, each as a bar followed by its
// figures. Every bar has the same width: what the content area leaves after
// the widest name and the widest figures, so the figures line up and no line
// wraps while there is room for a bar of the minimum width.
func (m *model) meterBlock(heading string, rows []meterRow) string {
	var cols meterColumns
	for _, r := range rows {
		cols.name = max(cols.name, len(r.name))
		cols.used = max(cols.used, len(r.format(r.used)))
		if r.limit > 0 {
			cols.limit = max(cols.limit, len(r.format(r.limit)))
		}
	}
	cols.name = min(cols.name, dashNameMax)
	figuresW := 0
	for _, r := range rows {
		figuresW = max(figuresW, lipgloss.Width(meterFigures(r, cols)))
	}
	// One space after the name, the bar's two brackets, and two spaces before
	// the figures.
	cols.bar = min(max(m.contentWidth()-cols.name-figuresW-5, dashBarMin), dashBarMax)

	lines := make([]string, 0, len(rows)+1)
	lines = append(lines, colHeaderStyle.Render(heading))
	for _, r := range rows {
		lines = append(lines, meterLine(r, cols))
	}
	return strings.Join(lines, "\n")
}

// meterColumns holds the widths that line a block's rows up: the name, the
// bar, and the used and limit figures.
type meterColumns struct {
	name  int
	bar   int
	used  int
	limit int
}

// meterLine renders a row's name, its usage bar and the figures behind it.
// A row with no limit has nothing to fill a bar against, so the bar's place
// says it is unlimited.
func meterLine(r meterRow, cols meterColumns) string {
	bar := pathStyle.Render(fmt.Sprintf("%-*s", cols.bar+2, "no limit"))
	if r.limit > 0 {
		bar = usageBar(humanize.Percent(r.used, r.limit), cols.bar)
	}
	return fmt.Sprintf("%-*s ", cols.name, truncate(r.name, cols.name)) + bar + "  " + meterFigures(r, cols)
}

// meterFigures renders what a row holds against its limit, then its markers.
// Each figure is padded to its column, so the percentages and markers line up
// down the block; a row with no limit leaves those columns blank.
func meterFigures(r meterRow, cols meterColumns) string {
	figures := fmt.Sprintf("%*s", cols.used, r.format(r.used))
	if r.limit > 0 {
		figures += fmt.Sprintf(" / %-*s  ", cols.limit, r.format(r.limit)) +
			percentText(humanize.Percent(r.used, r.limit), 4)
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
		field{"database", m.dashboardDB()},
		field{"replication", m.dashboardReplication()},
		field{"verified", integrityHeadline(iv) + deferredSuffix(iv.DeferredCopies)},
		field{"encryption", dashboardEncryption(iv.PlaintextCopies)},
		field{"compression", m.dashboardCompression()},
		field{"usage", m.dashboardUsage()},
		field{"cleanup", m.dashboardCleanup()},
	)
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

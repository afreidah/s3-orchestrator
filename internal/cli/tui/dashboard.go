// -------------------------------------------------------------------------------
// TUI - Dashboard
//
// Author: Alex Freidah
//
// The section the TUI opens on: a summary of the fleet, so an operator can see
// whether anything is wrong without visiting every pane. It shows capacity per
// backend and in total, the replication and verification backlogs, encryption
// and compression coverage, usage for the period, and the cleanup queues.
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

// dashLabelWidth aligns the labels of the summary block.
const dashLabelWidth = 14

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
	return m.frame(m.dashboardHeaderView(), m.footer("r reload - tab nav - q quit"), m.dashboardBody()...)
}

// dashboardHeaderView renders the title bar with the backend count.
func (m *model) dashboardHeaderView() string {
	title := fmt.Sprintf("dashboard   %d backends", len(m.backends.rows))
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title)
}

// dashboardBody renders the capacity block above the summary block. It waits
// for the first status snapshot, since every figure but replication comes
// from it; a status error is shown in its place.
func (m *model) dashboardBody() []pane {
	loading := m.dbHealthy == nil && m.backends.err == nil
	return m.paneBody(m.backends.err, "", loading, func() []pane {
		return []pane{textPane(m.dashboardCapacity()), textPane(""), textPane(m.dashboardSummary())}
	})
}

// capacityRow is one line of the capacity block: a backend, or the fleet
// total, which has no markers.
type capacityRow struct {
	name    string
	used    int64
	limit   int64
	markers string
}

// dashboardCapacity renders one usage bar per backend and one for the fleet,
// each followed by its used and limit bytes and, for backends, its health and
// drain state. Every bar has the same width: what the content area leaves
// after the widest name and the widest figures, so the figures line up and no
// line wraps while there is room for a bar of the minimum width.
func (m *model) dashboardCapacity() string {
	rows := make([]capacityRow, 0, len(m.backends.rows)+1)
	var used, limit int64
	for i := range m.backends.rows {
		b := &m.backends.rows[i]
		used += b.BytesUsed
		if b.BytesLimit > 0 {
			limit += b.BytesLimit
		}
		rows = append(rows, capacityRow{name: b.Name, used: b.BytesUsed, limit: b.BytesLimit, markers: backendMarkers(b)})
	}
	rows = append(rows, capacityRow{name: dashFleetLabel, used: used, limit: limit})

	var cols capacityColumns
	for _, r := range rows {
		cols.name = max(cols.name, len(r.name))
		cols.used = max(cols.used, len(humanize.Bytes(r.used)))
		if r.limit > 0 {
			cols.limit = max(cols.limit, len(humanize.Bytes(r.limit)))
		}
	}
	cols.name = min(cols.name, dashNameMax)
	figuresW := 0
	for _, r := range rows {
		figuresW = max(figuresW, lipgloss.Width(capacityFigures(r, cols)))
	}
	// One space after the name, the bar's two brackets, and two spaces before
	// the figures.
	cols.bar = min(max(m.contentWidth()-cols.name-figuresW-5, dashBarMin), dashBarMax)

	lines := make([]string, 0, len(rows)+1)
	lines = append(lines, colHeaderStyle.Render("capacity"))
	for _, r := range rows {
		lines = append(lines, capacityLine(r, cols))
	}
	return strings.Join(lines, "\n")
}

// capacityColumns holds the widths that line the capacity block's rows up:
// the name, the bar, and the used and limit figures.
type capacityColumns struct {
	name  int
	bar   int
	used  int
	limit int
}

// capacityLine renders a row's name, its usage bar and the figures behind it.
// A backend with no limit has nothing to fill a bar against, so the bar's
// place says it is unlimited.
func capacityLine(r capacityRow, cols capacityColumns) string {
	bar := pathStyle.Render(fmt.Sprintf("%-*s", cols.bar+2, "no limit"))
	if r.limit > 0 {
		bar = usageBar(usagePercent(r.used, r.limit), cols.bar)
	}
	return fmt.Sprintf("%-*s ", cols.name, truncate(r.name, cols.name)) + bar + "  " + capacityFigures(r, cols)
}

// capacityFigures renders what a row holds against its limit, then its
// markers. Each figure is padded to its column, so the percentages and markers
// line up down the block; a row with no limit leaves those columns blank.
func capacityFigures(r capacityRow, cols capacityColumns) string {
	figures := fmt.Sprintf("%*s", cols.used, humanize.Bytes(r.used))
	if r.limit > 0 {
		pct := usagePercent(r.used, r.limit)
		figures += fmt.Sprintf(" / %-*s  ", cols.limit, humanize.Bytes(r.limit)) +
			usageStyle(pct).Render(fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pct)))
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

// backendMarkers renders a backend's breaker health and, when it has one, its
// drain state, since either one means the backend is taking no new writes.
func backendMarkers(b *adminapi.BackendStatus) string {
	health := statusOKStyle.Render("healthy")
	if !b.Healthy {
		health = statusErrStyle.Render("UNHEALTHY")
	}
	if b.DrainState == "" {
		return health
	}
	return health + "  " + logLevelWarn.Render(b.DrainState)
}

// dashboardSummary renders the fleet's backlogs and coverage as an aligned
// label/value block.
func (m *model) dashboardSummary() string {
	line := func(label, value string) string {
		return fmt.Sprintf("%-*s %s", dashLabelWidth, label, value)
	}
	iv := m.backends.integrity
	lines := []string{
		line("database", m.dashboardDB()),
		line("replication", m.dashboardReplication()),
		line("verified", integrityHeadline(iv)+deferredSuffix(iv.DeferredCopies)),
		line("encryption", dashboardEncryption(iv.PlaintextCopies)),
		line("compression", m.dashboardCompression()),
		line("usage", m.dashboardUsage()),
		line("cleanup", m.dashboardCleanup()),
	}
	return strings.Join(lines, "\n")
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
		replCountStyle(s.UnderReplicated).Render(humanize.Comma(s.UnderReplicated)),
		replCountStyle(s.OverReplicated).Render(humanize.Comma(s.OverReplicated)))
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
	var saved int64
	for i := range m.backends.rows {
		saved += m.backends.rows[i].CompressionSavedBytes
	}
	if saved <= 0 {
		return pathStyle.Render("nothing stored compressed")
	}
	return statusOKStyle.Render(humanize.Bytes(saved)) + " saved"
}

// dashboardUsage renders the fleet's request and transfer totals for the
// current usage period.
func (m *model) dashboardUsage() string {
	var requests, ingress, egress int64
	for i := range m.backends.rows {
		b := &m.backends.rows[i]
		requests += b.APIRequests
		ingress += b.IngressBytes
		egress += b.EgressBytes
	}
	usage := fmt.Sprintf("%s requests   %s in   %s out",
		humanize.Comma(requests), humanize.Bytes(ingress), humanize.Bytes(egress))
	if m.backends.usagePeriod != "" {
		usage += "   " + pathStyle.Render("period "+m.backends.usagePeriod)
	}
	return usage
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

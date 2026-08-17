// -------------------------------------------------------------------------------
// TUI - Backends View
//
// Author: Alex Freidah
//
// Read-only status pane over the configured backends. Fetches the admin status
// snapshot and renders one row per backend: quota usage, object count,
// circuit-breaker health, drain state, and per-period API/egress/ingress
// counters. Reached with "b" from the browser; "esc" returns to the listing.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// backendsView holds the state of the backends status pane.
type backendsView struct {
	rows        []adminapi.BackendStatus // one entry per configured backend
	table       table.Model              // scrolling table over the backends
	dbHealthy   bool                     // metadata database health
	usagePeriod string                   // period the usage counters cover
	integrity   adminapi.IntegrityStatus // how far behind content verification is
	loading     bool                     // a status fetch is in flight
	err         error                    // last fetch error, if any
	drain       drainWatch               // the drain this pane is following, if any
}

// drainWatch follows one backend's drain. The endpoints are start, poll and
// cancel rather than a stream, so the pane polls progress on a ticker for as
// long as the drain stays active and renders the counts above the table.
type drainWatch struct {
	backend  string                          // backend being drained, "" when idle
	progress *adminapi.DrainProgressResponse // last polled progress, nil until the first poll
	ticking  bool                            // a poll is scheduled
	err      error                           // last poll or cancel error, if any
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// statusLoadedMsg carries a successfully loaded status snapshot.
type statusLoadedMsg struct{ resp *adminapi.StatusResponse }

// statusErrMsg carries a failed status fetch.
type statusErrMsg struct{ err error }

// drainPollInterval is how often the pane re-reads an active drain's progress.
// The drain endpoints are start/poll/cancel rather than a stream, so this is
// what makes the counts move.
const drainPollInterval = 2 * time.Second

// drainStartedMsg reports that a drain was accepted (or failed to start).
type drainStartedMsg struct {
	backend string
	err     error
}

// drainProgressMsg carries one polled progress reading.
type drainProgressMsg struct {
	backend  string
	progress *adminapi.DrainProgressResponse
	err      error
}

// drainTickMsg schedules the next progress poll.
type drainTickMsg struct{}

// drainCancelledMsg reports the outcome of cancelling a drain.
type drainCancelledMsg struct {
	backend string
	err     error
}

// backendReconciledMsg carries the outcome of reconciling one backend.
type backendReconciledMsg struct {
	backend string
	resp    *adminapi.ReconcileResponse
	err     error
}

// backendRequeuedMsg carries the outcome of requeueing one backend's
// dead-lettered cleanups.
type backendRequeuedMsg struct {
	backend string
	resp    *adminapi.CleanupDLQRequeueResponse
	err     error
}

// loadStatus returns a command that fetches the status snapshot off the main
// loop, delivering the result back as a statusLoadedMsg or statusErrMsg.
func (m *model) loadStatus() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.GetStatus(context.Background())
		if err != nil {
			return statusErrMsg{err}
		}
		return statusLoadedMsg{resp}
	}
}

// startDrain returns a command that asks the instance to drain one backend.
func (m *model) startDrain(backend string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		_, err := client.StartDrain(context.Background(), backend)
		return drainStartedMsg{backend: backend, err: err}
	}
}

// pollDrain returns a command that reads one backend's drain progress.
func (m *model) pollDrain(backend string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.DrainProgress(context.Background(), backend)
		return drainProgressMsg{backend: backend, progress: resp, err: err}
	}
}

// cancelDrain returns a command that aborts the drain on one backend.
func (m *model) cancelDrain(backend string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		_, err := client.CancelDrain(context.Background(), backend)
		return drainCancelledMsg{backend: backend, err: err}
	}
}

// reconcileBackend returns a command that reconciles metadata against one
// backend's storage.
func (m *model) reconcileBackend(backend string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.ReconcileBackend(context.Background(), backend)
		return backendReconciledMsg{backend: backend, resp: resp, err: err}
	}
}

// requeueBackendDLQ returns a command that requeues one backend's
// dead-lettered cleanup rows.
func (m *model) requeueBackendDLQ(backend string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.RequeueCleanupDLQ(context.Background(), backend)
		return backendRequeuedMsg{backend: backend, resp: resp, err: err}
	}
}

// drainTick schedules the next progress poll.
func drainTick() tea.Cmd {
	return tea.Tick(drainPollInterval, func(time.Time) tea.Msg { return drainTickMsg{} })
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyStatus folds a loaded snapshot into the backends state.
func (m *model) applyStatus(resp *adminapi.StatusResponse) {
	m.backends.rows = resp.Backends
	m.backends.dbHealthy = resp.DBHealthy
	m.backends.usagePeriod = resp.UsagePeriod
	m.backends.integrity = resp.Integrity
	healthy := resp.DBHealthy
	m.dbHealthy = &healthy // surface globally for the sidebar indicator
	m.backends.table.SetRows(rowsFromBackends(resp.Backends))
	m.backends.table.SetCursor(0)
	m.backends.loading = false
	m.backends.err = nil
}

// handleBackendsKey applies backends-level keys (quit, back, reload) and
// delegates cursor movement to the table.
func (m *model) handleBackendsKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "left", "h":
		return m.navBack()
	case "r":
		m.backends.loading = true
		cmd := m.loadStatus()
		return m, cmd
	case "d", "R", "Q", "x":
		return m.armBackendAction(key.String())
	}

	var cmd tea.Cmd
	m.backends.table, cmd = m.backends.table.Update(key)
	return m, cmd
}

// armBackendAction arms the action bound to key against the highlighted row.
// Every one names the backend in its confirmation, so a keystroke on the wrong
// row cannot start a drain on it.
func (m *model) armBackendAction(key string) (tea.Model, tea.Cmd) {
	name := m.selectedBackend()
	if name == "" {
		return m, nil
	}

	switch key {
	case "d":
		return m.startAction(adminAction{
			confirm: "Drain every copy off " + name + "?",
			before:  func(m *model) { m.beginDrainWatch(name) },
			run:     m.startDrain(name),
		})
	case "R":
		return m.startAction(adminAction{
			confirm: "Reconcile metadata against " + name + "?",
			run:     m.reconcileBackend(name),
		})
	case "Q":
		return m.startAction(adminAction{
			confirm: "Requeue dead-lettered cleanups for " + name + "?",
			run:     m.requeueBackendDLQ(name),
		})
	case "x":
		if m.backends.drain.backend != name {
			return m, nil
		}
		return m.startAction(adminAction{
			confirm: "Cancel the drain on " + name + "? Copies already moved stay moved.",
			run:     m.cancelDrain(name),
		})
	}
	return m, nil
}

// selectedBackend names the highlighted row, or "" when the table is empty.
func (m *model) selectedBackend() string {
	cursor := m.backends.table.Cursor()
	if cursor < 0 || cursor >= len(m.backends.rows) {
		return ""
	}
	return m.backends.rows[cursor].Name
}

// -------------------------------------------------------------------------
// DRAIN TRANSITIONS
// -------------------------------------------------------------------------

// updateBackends handles the messages this pane raises for itself, reporting
// whether the message was one of them so the model's own switch stays about
// everything else.
func (m *model) updateBackends(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case drainStartedMsg:
		model, cmd := m.applyDrainStarted(msg)
		return model, cmd, true
	case drainProgressMsg:
		model, cmd := m.applyDrainProgress(msg)
		return model, cmd, true
	case drainTickMsg:
		model, cmd := m.onDrainTick()
		return model, cmd, true
	case drainCancelledMsg:
		model, cmd := m.applyDrainCancelled(msg)
		return model, cmd, true
	case backendReconciledMsg:
		model, cmd := m.applyBackendReconciled(msg)
		return model, cmd, true
	case backendRequeuedMsg:
		model, cmd := m.applyBackendRequeued(msg)
		return model, cmd, true
	}
	return m, nil, false
}

// beginDrainWatch shows the pane following a drain the moment it is accepted,
// so a long migration reports that it started rather than looking inert until
// the first poll lands.
func (m *model) beginDrainWatch(backend string) {
	m.backends.drain = drainWatch{backend: backend}
}

// applyDrainStarted reports whether the drain was accepted and, if so, begins
// polling its progress.
func (m *model) applyDrainStarted(msg drainStartedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.backends.drain = drainWatch{}
		m.status = &actionStatus{ok: false, text: "drain " + msg.backend + ": " + msg.err.Error()}
		return m, nil
	}
	m.status = &actionStatus{ok: true, text: "draining " + msg.backend}
	m.backends.drain.ticking = true
	return m, tea.Batch(m.pollDrain(msg.backend), drainTick())
}

// applyDrainProgress folds one polled reading in. A drain that is no longer
// active has finished or was cancelled, so the watch ends and the status
// snapshot is refreshed to clear the row's DRAIN flag.
func (m *model) applyDrainProgress(msg drainProgressMsg) (tea.Model, tea.Cmd) {
	if msg.backend != m.backends.drain.backend {
		return m, nil // a stale reading for a drain the pane already stopped following
	}
	if msg.err != nil {
		m.backends.drain.err = msg.err
		return m, nil
	}

	m.backends.drain.progress = msg.progress
	m.backends.drain.err = nil
	if msg.progress != nil && !msg.progress.Active {
		m.backends.drain = drainWatch{}
		m.status = &actionStatus{ok: true, text: "drain finished on " + msg.backend}
		refresh := m.loadStatus()
		return m, refresh
	}
	return m, nil
}

// onDrainTick polls again while a drain is still being followed. The ticker
// lapses once the drain ends or the operator leaves the pane.
func (m *model) onDrainTick() (tea.Model, tea.Cmd) {
	if m.backends.drain.backend == "" || m.section != sectionBackends {
		m.backends.drain.ticking = false
		return m, nil
	}
	return m, tea.Batch(m.pollDrain(m.backends.drain.backend), drainTick())
}

// applyDrainCancelled ends the watch and refreshes the snapshot, so the row
// stops reporting itself as draining.
func (m *model) applyDrainCancelled(msg drainCancelledMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = &actionStatus{ok: false, text: "cancel drain " + msg.backend + ": " + msg.err.Error()}
		return m, nil
	}
	m.backends.drain = drainWatch{}
	m.status = &actionStatus{ok: true, text: "drain cancelled on " + msg.backend}
	refresh := m.loadStatus()
	return m, refresh
}

// applyBackendReconciled reports what reconciling one backend changed.
func (m *model) applyBackendReconciled(msg backendReconciledMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = &actionStatus{ok: false, text: "reconcile " + msg.backend + ": " + msg.err.Error()}
		return m, nil
	}
	m.status = &actionStatus{ok: true, text: fmt.Sprintf("reconciled %s: imported %d, removed %d",
		msg.backend, msg.resp.Imported, msg.resp.Removed)}
	refresh := m.loadStatus()
	return m, refresh
}

// applyBackendRequeued reports how many dead-lettered cleanups went back on
// the queue for one backend.
func (m *model) applyBackendRequeued(msg backendRequeuedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = &actionStatus{ok: false, text: "requeue " + msg.backend + ": " + msg.err.Error()}
		return m, nil
	}
	m.status = &actionStatus{ok: true, text: fmt.Sprintf("requeued %d for %s", msg.resp.Requeued, msg.backend)}
	return m, nil
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// resizeBackends fits the backends columns and viewport to the window, capping
// the name column so short backend names don't sprawl on a wide terminal.
func (m *model) resizeBackends() {
	const (
		fixed   = 9 + 8 + 12 + 12 + 6 + 10 + 10 + 12 + 12 // health..egress incl use%
		cols    = 10
		nameCap = 24
	)
	nameWidth := fitFirstColumn(m.contentWidth(), fixed, cols, nameCap)
	m.backends.table.SetColumns([]table.Column{
		{Title: "BACKEND", Width: nameWidth},
		{Title: "HEALTH", Width: 9},
		{Title: "DRAIN", Width: 8},
		{Title: "USED", Width: 12},
		{Title: "LIMIT", Width: 12},
		{Title: "USE%", Width: 6},
		{Title: "OBJECTS", Width: 10},
		{Title: "API", Width: 10},
		{Title: "INGRESS", Width: 12},
		{Title: "EGRESS", Width: 12},
	})
	m.backends.table.SetWidth(m.contentWidth())
	m.backends.table.SetHeight(max(m.height-3, 3)) // 2-line header + 1-line footer
}

// rowsFromBackends builds table rows from the status snapshot, in the same
// order so the table cursor indexes straight into rows.
func rowsFromBackends(backends []adminapi.BackendStatus) []table.Row {
	rows := make([]table.Row, 0, len(backends))
	for i := range backends {
		b := backends[i]
		limit, usePct := "-", "-"
		if b.BytesLimit > 0 {
			limit = humanize.Bytes(b.BytesLimit)
			usePct = fmt.Sprintf("%d%%", usagePercent(b.BytesUsed, b.BytesLimit))
		}
		rows = append(rows, table.Row{
			b.Name,
			backendHealth(b.Healthy),
			backendDrain(b.Draining),
			humanize.Bytes(b.BytesUsed),
			limit,
			usePct,
			strconv.FormatInt(b.ObjectCount, 10),
			strconv.FormatInt(b.APIRequests, 10),
			humanize.Bytes(b.IngressBytes),
			humanize.Bytes(b.EgressBytes),
		})
	}
	return rows
}

// backendHealth renders a backend's circuit-breaker state.
func backendHealth(healthy bool) string {
	if healthy {
		return "healthy"
	}
	return "unhealthy"
}

// backendDrain renders whether a backend is draining.
func backendDrain(draining bool) string {
	if draining {
		return "draining"
	}
	return "-"
}

// backendsView composes the pane's full-screen layout.
func (m *model) backendsPaneView() string {
	return m.frame(m.backendsHeaderView(), m.backendsFooterView(), m.backendsBodyView())
}

// backendsHeaderView renders the title bar with the backend count and DB health.
func (m *model) backendsHeaderView() string {
	title := fmt.Sprintf("backends   %d configured", len(m.backends.rows))
	if m.backends.usagePeriod != "" {
		title += "   usage period: " + m.backends.usagePeriod
	}
	titleLine := m.contentTitleStyle().Width(m.contentWidth()).Render(title)
	header := titleLine + "\n" + m.backendsStatsLine()
	if line := m.drainProgressLine(); line != "" {
		header += "\n" + line
	}
	return header
}

// drainProgressLine reports the drain the pane is following. The DRAIN column
// only says whether a backend is draining, so the counts live here, where they
// can move without redrawing the table.
func (m *model) drainProgressLine() string {
	watch := m.backends.drain
	if watch.backend == "" {
		return ""
	}
	if watch.err != nil {
		return errStyle.Render("drain " + watch.backend + ": " + watch.err.Error())
	}
	if watch.progress == nil {
		return pathStyle.Render("draining " + watch.backend + "   starting...")
	}
	return pathStyle.Render(fmt.Sprintf("draining %s   moved %s   remaining %s (%s)",
		watch.backend,
		grouped(int(watch.progress.ObjectsMoved)),
		grouped(int(watch.progress.ObjectsRemaining)),
		humanize.Bytes(watch.progress.BytesRemaining)))
}

// backendsStatsLine renders the coloured DB-health + total-usage line beneath
// the backends title. Kept out of the title bar so the colours are not fighting
// its background.
func (m *model) backendsStatsLine() string {
	db := statusOKStyle.Render("healthy")
	if !m.backends.dbHealthy {
		db = statusErrStyle.Render("UNAVAILABLE")
	}

	var used, limit int64
	for i := range m.backends.rows {
		used += m.backends.rows[i].BytesUsed
		if m.backends.rows[i].BytesLimit > 0 {
			limit += m.backends.rows[i].BytesLimit
		}
	}
	total := "total: " + humanize.Bytes(used)
	if limit > 0 {
		pct := usagePercent(used, limit)
		total = fmt.Sprintf("total: %s / %s (%s)",
			humanize.Bytes(used), humanize.Bytes(limit), usageStyle(pct).Render(fmt.Sprintf("%d%%", pct)))
	}
	return fmt.Sprintf("db: %s   %s   %s%s", db, total, m.integrityCoverage(), m.encryptionCoverage())
}

// encryptionCoverage renders how much of the fleet is still plaintext, and
// nothing at all once none of it is. Encryption applies to new writes, so a
// non-zero count means existing objects were never rewritten.
func (m *model) encryptionCoverage() string {
	plaintext := m.backends.integrity.PlaintextCopies
	if plaintext <= 0 {
		return ""
	}
	return "   plaintext: " + statusErrStyle.Render(humanize.Comma(plaintext))
}

// integrityCoverage renders how far behind verification is. Never-verified
// copies read as a warning because they are the ones a scrub has never seen.
func (m *model) integrityCoverage() string {
	iv := m.backends.integrity
	if iv.NeverVerifiedCopies > 0 {
		return "verified: " + statusErrStyle.Render(
			fmt.Sprintf("%s never", humanize.Comma(iv.NeverVerifiedCopies)))
	}
	if iv.OldestUnverifiedSeconds <= 0 {
		return "verified: " + statusOKStyle.Render("up to date")
	}
	return "verified: oldest " + humanize.Duration(
		time.Duration(iv.OldestUnverifiedSeconds)*time.Second)
}

// usagePercent returns used as a whole-number percentage of limit (0 when
// limit is non-positive).
func usagePercent(used, limit int64) int {
	if limit <= 0 {
		return 0
	}
	return int(used * 100 / limit)
}

// backendsFooterView renders the backends key hints. The cancel key is offered
// only while this pane is following a drain, so it cannot read as available
// when there is nothing to stop.
func (m *model) backendsFooterView() string {
	hints := "up/down move - d drain - R reconcile - Q requeue dlq - r reload - tab nav - q quit"
	if m.backends.drain.backend != "" {
		hints = "x cancel drain - " + hints
	}
	return m.footer(hints)
}

// backendsBodyView renders the current content: an error, the loading
// indicator, an empty notice, or the backends table.
func (m *model) backendsBodyView() string {
	return m.paneBody(m.backends.err, "", m.backends.loading, func() string {
		if len(m.backends.rows) == 0 {
			return pathStyle.Render("(no backends)")
		}
		return m.backends.table.View()
	})
}

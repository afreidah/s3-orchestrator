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
	list        sortTable[adminapi.BackendStatus] // one row per configured backend, in server order until sorted
	dbHealthy   bool                              // metadata database health
	usagePeriod string                            // period the usage counters cover
	integrity   adminapi.IntegrityStatus          // how far behind content verification is
	loading     bool                              // a status fetch is in flight
	err         error                             // last fetch error, if any
	drain       drainWatch                        // the drain this pane is following, if any
}

// newBackendsView builds the pane's empty state.
func newBackendsView() backendsView {
	return backendsView{list: newSortTable(backendColumns, backendSorts, rowsFromBackends,
		func(b *adminapi.BackendStatus) string { return b.Name })}
}

// drainWatch follows one backend's drain. The endpoints are start, poll and
// cancel rather than a stream, so the poller reads progress for as long as the
// drain stays active, whichever pane is showing, and the backends pane renders
// the counts above the table.
type drainWatch struct {
	backend   string                          // backend being drained, "" when idle
	progress  *adminapi.DrainProgressResponse // last polled progress, nil until the first poll
	following bool                            // the drain was accepted and the poller reads its progress
	err       error                           // last poll or cancel error, if any
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// statusLoadedMsg carries a successfully loaded status snapshot.
type statusLoadedMsg struct{ resp *adminapi.StatusResponse }

// statusErrMsg carries a failed status fetch.
type statusErrMsg struct{ err error }

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

// backendCmd builds the command every per-backend admin action is: call the
// endpoint off the main loop and hand the result to toMsg.
//
// Callers pass a bound method value, so the client is read while Update still
// holds the model. The returned function runs afterwards and must not touch
// the model at all.
func backendCmd[T any](backend string, call func(context.Context, string) (T, error), toMsg func(T, error) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		resp, err := call(context.Background(), backend)
		return toMsg(resp, err)
	}
}

// startDrain returns a command that asks the instance to drain one backend.
func (m *model) startDrain(backend string) tea.Cmd {
	return backendCmd(backend, m.client.StartDrain,
		func(_ *adminapi.BackendOperationResponse, err error) tea.Msg {
			return drainStartedMsg{backend: backend, err: err}
		})
}

// pollDrain returns a command that reads one backend's drain progress.
func (m *model) pollDrain(backend string) tea.Cmd {
	return backendCmd(backend, m.client.DrainProgress,
		func(resp *adminapi.DrainProgressResponse, err error) tea.Msg {
			return drainProgressMsg{backend: backend, progress: resp, err: err}
		})
}

// cancelDrain returns a command that aborts the drain on one backend.
func (m *model) cancelDrain(backend string) tea.Cmd {
	return backendCmd(backend, m.client.CancelDrain,
		func(_ *adminapi.BackendOperationResponse, err error) tea.Msg {
			return drainCancelledMsg{backend: backend, err: err}
		})
}

// reconcileBackend returns a command that reconciles metadata against one
// backend's storage.
func (m *model) reconcileBackend(backend string) tea.Cmd {
	return backendCmd(backend, m.client.ReconcileBackend,
		func(resp *adminapi.ReconcileResponse, err error) tea.Msg {
			return backendReconciledMsg{backend: backend, resp: resp, err: err}
		})
}

// requeueBackendDLQ returns a command that requeues one backend's
// dead-lettered cleanup rows.
func (m *model) requeueBackendDLQ(backend string) tea.Cmd {
	return backendCmd(backend, m.client.RequeueCleanupDLQ,
		func(resp *adminapi.CleanupDLQRequeueResponse, err error) tea.Msg {
			return backendRequeuedMsg{backend: backend, resp: resp, err: err}
		})
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyStatus folds a loaded snapshot into the backends state, keeping the
// highlighted backend selected across the refresh.
func (m *model) applyStatus(resp *adminapi.StatusResponse) {
	m.backends.list.setItems(resp.Backends)
	m.backends.dbHealthy = resp.DBHealthy
	m.backends.usagePeriod = resp.UsagePeriod
	m.backends.integrity = resp.Integrity
	healthy := resp.DBHealthy
	m.dbHealthy = &healthy // surface globally for the sidebar indicator
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
		m.backends.loading = !m.backends.list.loaded()
		cmd := m.fetch(pollStatus)
		return m, cmd
	case "d", "R", "Q", "x":
		return m.armBackendAction(key.String())
	case "a", "enter", "right", "l":
		return m.openBackendActions()
	}

	cmd := m.backends.list.update(key)
	return m, cmd
}

// openBackendActions shows the highlighted backend's action menu, which is the
// ops pane scoped to that one backend.
//
// The menu is where the passes that read and rewrite a backend's copies live,
// rather than more single letters on this pane: each names what it will do in
// full and confirms against the backend, which a keystroke on the wrong row
// cannot do.
func (m *model) openBackendActions() (tea.Model, tea.Cmd) {
	name := m.selectedBackend()
	if name == "" {
		return m, nil
	}
	m.section = sectionOps
	m.navFocus = false
	m.ops = opsView{actions: backendActions(), backend: name}
	return m, nil
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
	return m.backends.list.selectedKey()
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

// applyDrainStarted reports whether the drain was accepted and, if so, hands
// its progress to the poller, which reads it on every pane until it ends.
func (m *model) applyDrainStarted(msg drainStartedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.backends.drain = drainWatch{}
		m.report(false, "drain "+msg.backend+": "+msg.err.Error())
		return m, nil
	}
	m.report(true, "draining "+msg.backend)
	m.backends.drain.following = true
	cmd := m.fetch(pollDrain)
	return m, cmd
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
		m.report(drainEndStatus(msg.backend, msg.progress))
		refresh := m.fetch(pollStatus)
		return m, refresh
	}
	return m, nil
}

// drainEndStatus reports how a followed drain ended: finished, or failed with
// the reason the drain recorded.
func drainEndStatus(backend string, p *adminapi.DrainProgressResponse) (ok bool, text string) {
	if p.State == "failed" {
		return false, "drain failed on " + backend + ": " + p.Error
	}
	return true, "drain finished on " + backend
}

// applyDrainCancelled ends the watch and refreshes the snapshot, so the row
// stops reporting itself as draining.
func (m *model) applyDrainCancelled(msg drainCancelledMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.report(false, "cancel drain "+msg.backend+": "+msg.err.Error())
		return m, nil
	}
	m.backends.drain = drainWatch{}
	m.report(true, "drain cancelled on "+msg.backend)
	refresh := m.fetch(pollStatus)
	return m, refresh
}

// applyBackendReconciled reports what reconciling one backend changed.
func (m *model) applyBackendReconciled(msg backendReconciledMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.report(false, "reconcile "+msg.backend+": "+msg.err.Error())
		return m, nil
	}
	m.report(true, fmt.Sprintf("reconciled %s: imported %d, removed %d",
		msg.backend, msg.resp.Imported, msg.resp.Removed))
	refresh := m.fetch(pollStatus)
	return m, refresh
}

// applyBackendRequeued reports how many dead-lettered cleanups went back on
// the queue for one backend.
func (m *model) applyBackendRequeued(msg backendRequeuedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.report(false, "requeue "+msg.backend+": "+msg.err.Error())
		return m, nil
	}
	m.report(true, fmt.Sprintf("requeued %d for %s", msg.resp.Requeued, msg.backend))
	return m, nil
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// backendColumns declares the backends table's columns. The name is capped so
// short backend names don't sprawl on a wide terminal. A narrow terminal keeps
// whether each backend is up, how full it is, and whether it is draining, and
// drops the period counters first.
var backendColumns = []columnSpec{
	{title: "BACKEND", min: 8, max: 24, priority: 11},
	{title: "HEALTH", min: 9, max: 9, priority: 10},
	{title: "DRAIN", min: 8, max: 8, priority: 8},
	{title: "USED", min: 12, max: 12, priority: 7},
	{title: "LIMIT", min: 12, max: 12, priority: 5},
	{title: "USE%", min: 6, max: 6, priority: 9},
	{title: "OBJECTS", min: 10, max: 10, priority: 6},
	{title: "API", min: 10, max: 10, priority: 2},
	{title: "INGRESS", min: 12, max: 12, priority: 3},
	{title: "EGRESS", min: 12, max: 12, priority: 4},
	{title: "SAVED", min: 12, max: 12, priority: 1},
}

// backendSorts orders the backends table by every column. A backend with no
// limit sorts as 0% used, and a healthy backend after an unhealthy one, so an
// ascending sort puts the backends needing attention first.
var backendSorts = map[string]func(a, b *adminapi.BackendStatus) int{
	"BACKEND": by(func(b *adminapi.BackendStatus) string { return b.Name }),
	"HEALTH":  by(func(b *adminapi.BackendStatus) int { return rank(b.Healthy) }),
	"DRAIN":   by(func(b *adminapi.BackendStatus) string { return b.DrainState }),
	"USED":    by(func(b *adminapi.BackendStatus) int64 { return b.BytesUsed }),
	"LIMIT":   by(func(b *adminapi.BackendStatus) int64 { return b.BytesLimit }),
	"USE%":    by(func(b *adminapi.BackendStatus) int { return usagePercent(b.BytesUsed, b.BytesLimit) }),
	"OBJECTS": by(func(b *adminapi.BackendStatus) int64 { return b.ObjectCount }),
	"API":     by(func(b *adminapi.BackendStatus) int64 { return b.APIRequests }),
	"INGRESS": by(func(b *adminapi.BackendStatus) int64 { return b.IngressBytes }),
	"EGRESS":  by(func(b *adminapi.BackendStatus) int64 { return b.EgressBytes }),
	"SAVED":   by(func(b *adminapi.BackendStatus) int64 { return b.CompressionSavedBytes }),
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
			backendDrain(b.DrainState),
			humanize.Bytes(b.BytesUsed),
			limit,
			usePct,
			strconv.FormatInt(b.ObjectCount, 10),
			strconv.FormatInt(b.APIRequests, 10),
			humanize.Bytes(b.IngressBytes),
			humanize.Bytes(b.EgressBytes),
			savedBytes(b.CompressionSavedBytes),
		})
	}
	return rows
}

// savedBytes renders what compression saved on a backend. A dash rather than
// "0 B" for nothing, since on a fleet with compression off that is every row
// and a column of zeroes reads as a broken figure.
func savedBytes(saved int64) string {
	if saved <= 0 {
		return "-"
	}
	return humanize.Bytes(saved)
}

// backendHealth renders a backend's circuit-breaker state.
func backendHealth(healthy bool) string {
	if healthy {
		return "healthy"
	}
	return "unhealthy"
}

// backendDrain renders a backend's drain state: draining, drained, or failed,
// and a dash when it has no drain.
func backendDrain(state string) string {
	if state == "" {
		return "-"
	}
	return state
}

// backendsView composes the pane's full-screen layout.
func (m *model) backendsPaneView() string {
	return m.frame(m.backendsHeaderView(), m.hintFooter(), m.backendsBody()...)
}

// backendsHeaderView renders the title bar with the backend count and DB health.
func (m *model) backendsHeaderView() string {
	title := fmt.Sprintf("backends   %d configured", len(m.backends.list.received))
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
	for i := range m.backends.list.received {
		used += m.backends.list.received[i].BytesUsed
		if m.backends.list.received[i].BytesLimit > 0 {
			limit += m.backends.list.received[i].BytesLimit
		}
	}
	total := "total: " + humanize.Bytes(used)
	if limit > 0 {
		pct := usagePercent(used, limit)
		total = fmt.Sprintf("total: %s / %s (%s)",
			humanize.Bytes(used), humanize.Bytes(limit), usageStyle(pct).Render(fmt.Sprintf("%d%%", pct)))
	}
	return fmt.Sprintf("db: %s   %s   %s%s%s",
		db, total, m.integrityCoverage(), m.encryptionCoverage(), m.compressionCoverage())
}

// compressionCoverage renders what compression is saving across the fleet, and
// nothing at all when nothing is stored encoded. It keys off the saving rather
// than the setting: a fleet that has just enabled compression has nothing to
// report yet, and one that has just disabled it still holds everything it
// compressed.
func (m *model) compressionCoverage() string {
	var saved int64
	for i := range m.backends.list.received {
		saved += m.backends.list.received[i].CompressionSavedBytes
	}
	if saved <= 0 {
		return ""
	}
	return "   compression saved: " + statusOKStyle.Render(humanize.Bytes(saved))
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
// copies read as a warning because they are the ones a scrub has never seen,
// and deferred copies are appended because no sweep can reach them, so the
// figure ahead of them describes only part of the fleet.
func (m *model) integrityCoverage() string {
	iv := m.backends.integrity
	return "verified: " + integrityHeadline(iv) + deferredSuffix(iv.DeferredCopies)
}

// integrityHeadline is the reachable half of the coverage line, without its
// label, so the backends stats line and the dashboard can each label it.
func integrityHeadline(iv adminapi.IntegrityStatus) string {
	if iv.NeverVerifiedCopies > 0 {
		return statusErrStyle.Render(fmt.Sprintf("%s never", humanize.Comma(iv.NeverVerifiedCopies)))
	}
	if iv.OldestUnverifiedSeconds <= 0 {
		return statusOKStyle.Render("up to date")
	}
	return "oldest " + humanize.Duration(time.Duration(iv.OldestUnverifiedSeconds)*time.Second)
}

// deferredSuffix names the copies no sweep can reach, or nothing when the whole
// fleet is within its read budget.
func deferredSuffix(deferred int64) string {
	if deferred <= 0 {
		return ""
	}
	return "   " + statusErrStyle.Render(
		fmt.Sprintf("%s unreachable", humanize.Comma(deferred)))
}

// usagePercent returns used as a whole-number percentage of limit (0 when
// limit is non-positive).
func usagePercent(used, limit int64) int {
	if limit <= 0 {
		return 0
	}
	return int(used * 100 / limit)
}

// backendsBody renders the current content: an error, the loading indicator,
// an empty notice, or the backends table.
func (m *model) backendsBody() []pane {
	return m.paneBody(m.backends.err, "", m.backends.loading, func() []pane {
		if len(m.backends.list.received) == 0 {
			return []pane{textPane(pathStyle.Render("(no backends)"))}
		}
		return []pane{m.backends.list.pane(m)}
	})
}

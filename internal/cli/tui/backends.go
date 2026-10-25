// -------------------------------------------------------------------------------
// TUI - Backends View
//
// Author: Alex Freidah
//
// Status pane over the configured backends. Fetches the admin status snapshot
// and renders one row per backend - health, state flags, storage and object
// count - above a detail block for the highlighted backend: its configuration,
// every usage limit against its ceiling, and the instance whose view it is.
// The detail follows the cursor. Reached with "b"; "esc" returns focus to the
// nav.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// backendsView holds the state of the backends status pane.
type backendsView struct {
	list      sortTable[adminapi.BackendStatus] // one row per configured backend, in server order until sorted
	dbHealthy bool                              // metadata database health
	integrity adminapi.IntegrityStatus          // how far behind content verification is
	instance  adminapi.InstanceStatus           // the instance that answered the last status fetch
	loading   bool                              // a status fetch is in flight
	err       error                             // last fetch error, if any
	drain     drainWatch                        // the drain this pane is following, if any
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

// loadStatus returns a command that fetches the status snapshot off the main
// loop, delivering the result back as a statusLoadedMsg or statusErrMsg.
func (m *model) loadStatus() tea.Cmd {
	return fetchCmd(m.client.GetStatus,
		func(r *adminapi.StatusResponse) tea.Msg { return statusLoadedMsg{r} },
		func(err error) tea.Msg { return statusErrMsg{err} })
}

// fetchCmd calls get off the main loop and delivers loaded with the response,
// or failed with the error. Every pane that loads one snapshot builds its
// command with it; the returned function must not touch the model.
func fetchCmd[T any](get func(context.Context) (T, error), loaded func(T) tea.Msg, failed func(error) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		resp, err := get(context.Background())
		if err != nil {
			return failed(err)
		}
		return loaded(resp)
	}
}

// backendCmd calls a per-backend endpoint off the main loop and hands the
// result to toMsg. The returned function must not touch the model.
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

// cancelDrain returns a command that aborts the drain on one backend, ends the
// pane's watch of it, and refreshes the status snapshot to clear its flag.
func (m *model) cancelDrain(backend string) tea.Cmd {
	client := m.client
	return runAction("cancel drain "+backend,
		func(ctx context.Context) (*adminapi.BackendOperationResponse, error) {
			return client.CancelDrain(ctx, backend)
		},
		func(m *model, _ *adminapi.BackendOperationResponse) (string, tea.Cmd) {
			m.backends.drain = drainWatch{}
			return "drain cancelled on " + backend, m.fetch(pollStatus)
		})
}

// setCordon returns a command that cordons one backend when cordoned is true
// and uncordons it when false, then refreshes the status snapshot.
func (m *model) setCordon(backend string, cordoned bool) tea.Cmd {
	client := m.client
	verb := "uncordon"
	if cordoned {
		verb = "cordon"
	}
	return runAction(verb+" "+backend,
		func(ctx context.Context) (*adminapi.BackendOperationResponse, error) {
			return client.SetCordon(ctx, backend, cordoned)
		},
		func(m *model, _ *adminapi.BackendOperationResponse) (string, tea.Cmd) {
			return verb + "ed " + backend, m.fetch(pollStatus)
		})
}

// reconcileBackend returns a command that reconciles metadata against one
// backend's storage and reports what it changed.
func (m *model) reconcileBackend(backend string) tea.Cmd {
	client := m.client
	return runAction("reconcile "+backend,
		func(ctx context.Context) (*adminapi.ReconcileResponse, error) {
			return client.ReconcileBackend(ctx, backend)
		},
		func(m *model, resp *adminapi.ReconcileResponse) (string, tea.Cmd) {
			return fmt.Sprintf("reconciled %s: imported %d, removed %d", backend, resp.Imported, resp.Removed),
				m.fetch(pollStatus)
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
	m.backends.integrity = resp.Integrity
	m.backends.instance = resp.Instance
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
	case "d", "R", "Q", "x", "C":
		return m.armBackendAction(key.String())
	case "a", "enter", "right", "l":
		return m.openBackendActions()
	}

	cmd := m.backends.list.update(key)
	return m, cmd
}

// openBackendActions shows the highlighted backend's action menu, which is the
// ops pane scoped to that one backend. Each entry confirms against the named
// backend before it runs.
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
			run:     m.requeueDLQ(name),
		})
	case "x":
		if m.backends.drain.backend != name {
			return m, nil
		}
		return m.startAction(adminAction{
			confirm: "Cancel the drain on " + name + "? Copies already moved stay moved.",
			run:     m.cancelDrain(name),
		})
	case "C":
		if m.selectedCordoned() {
			return m.startAction(adminAction{
				confirm: "Uncordon " + name + "? New copies can land on it again.",
				run:     m.setCordon(name, false),
			})
		}
		return m.startAction(adminAction{
			confirm: "Cordon " + name + "? New copies stop landing on it; reads continue.",
			run:     m.setCordon(name, true),
		})
	}
	return m, nil
}

// selectedBackend names the highlighted row, or "" when the table is empty.
func (m *model) selectedBackend() string {
	return m.backends.list.selectedKey()
}

// flagCordoned is the status response's name for the cordon flag.
const flagCordoned = "cordoned"

// selectedCordoned reports whether the highlighted backend is cordoned, which
// decides whether C cordons it or uncordons it.
func (m *model) selectedCordoned() bool {
	b, ok := m.backends.list.selected()
	return ok && slices.Contains(b.Flags, flagCordoned)
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

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// backendColumns declares the backends table's columns. The name is capped so
// short backend names don't sprawl on a wide terminal. A narrow terminal keeps
// whether each backend is up, how full it is, and which state flags it carries.
// The usage figures and refused calls live in the detail below the table.
var backendColumns = []columnSpec{
	{title: "BACKEND", min: 8, max: 24, priority: 7},
	{title: "HEALTH", min: 9, max: 9, priority: 6},
	{title: "STATE", min: 8, max: 21, priority: 4},
	{title: "USED", min: 12, max: 12, priority: 3},
	{title: "LIMIT", min: 12, max: 12, priority: 1},
	{title: "USE%", min: 6, max: 6, priority: 5},
	{title: "OBJECTS", min: 10, max: 10, priority: 2},
}

// backendSorts orders the backends table by every column. A backend with no
// limit sorts as 0% used, and a healthy backend after a degraded or down one, so an
// ascending sort puts the backends needing attention first.
var backendSorts = map[string]func(a, b *adminapi.BackendStatus) int{
	"BACKEND": by(func(b *adminapi.BackendStatus) string { return b.Name }),
	"HEALTH":  by(func(b *adminapi.BackendStatus) int { return healthRank(b.RefusedCalls) }),
	"STATE":   by(func(b *adminapi.BackendStatus) string { return output.NameList(b.Flags) }),
	"USED":    by(func(b *adminapi.BackendStatus) int64 { return b.BytesUsed }),
	"LIMIT":   by(func(b *adminapi.BackendStatus) int64 { return b.BytesLimit }),
	"USE%":    by(func(b *adminapi.BackendStatus) int { return humanize.Percent(b.BytesUsed, b.BytesLimit) }),
	"OBJECTS": by(func(b *adminapi.BackendStatus) int64 { return b.ObjectCount }),
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
			usePct = fmt.Sprintf("%d%%", humanize.Percent(b.BytesUsed, b.BytesLimit))
		}
		rows = append(rows, table.Row{
			b.Name,
			core.CallsNamed(b.RefusedCalls).Health(),
			output.NameList(b.Flags),
			humanize.Bytes(b.BytesUsed),
			limit,
			usePct,
			strconv.FormatInt(b.ObjectCount, 10),
		})
	}
	return rows
}

// healthRank orders backends by health for sorting: down first, then
// degraded, then healthy, so an ascending sort leads with the worst.
func healthRank(refused []string) int {
	switch core.CallsNamed(refused).Health() {
	case core.HealthDown:
		return 0
	case core.HealthDegraded:
		return 1
	default:
		return 2
	}
}

// backendsView composes the pane's full-screen layout.
func (m *model) backendsPaneView() string {
	return m.frame(m.backendsHeaderView(), m.hintFooter(), m.backendsBody()...)
}

// backendsHeaderView renders the title bar with the backend count and DB health.
func (m *model) backendsHeaderView() string {
	title := fmt.Sprintf("backends   %d configured", len(m.backends.list.received))
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

	used, limit := m.fleetStorage()
	total := "total: " + humanize.Bytes(used)
	if limit > 0 {
		total = fmt.Sprintf("total: %s / %s (%s)",
			humanize.Bytes(used), humanize.Bytes(limit), percentText(humanize.Percent(used, limit), 0))
	}
	return fmt.Sprintf("db: %s   %s   %s%s%s",
		db, total, m.integrityCoverage(), m.encryptionCoverage(), m.compressionCoverage())
}

// fleetStorage sums the bytes every backend holds and the quotas of those that
// have one. A backend with no quota adds nothing to the limit.
func (m *model) fleetStorage() (used, limit int64) {
	for i := range m.backends.list.received {
		b := &m.backends.list.received[i]
		used += b.BytesUsed
		limit += max(b.BytesLimit, 0)
	}
	return used, limit
}

// fleetCompressionSaved sums what compression saves on every backend.
func (m *model) fleetCompressionSaved() int64 {
	var saved int64
	for i := range m.backends.list.received {
		saved += m.backends.list.received[i].CompressionSavedBytes
	}
	return saved
}

// compressionCoverage renders what compression is saving across the fleet, and
// nothing at all when nothing is stored encoded. It keys off the saving rather
// than the setting: a fleet that has just enabled compression has nothing to
// report yet, and one that has just disabled it still holds everything it
// compressed.
func (m *model) compressionCoverage() string {
	saved := m.fleetCompressionSaved()
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

// integrityCoverage renders how far behind verification is, with deferred
// copies appended since no sweep can reach them.
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

// backendsBody renders the current content: an error, the loading indicator,
// an empty notice, or the backends table with the highlighted backend's detail
// below it. The detail is drawn from the cursor on every render, so it follows
// the selection without a key of its own.
func (m *model) backendsBody() []pane {
	return m.paneBody(m.backends.err, "", m.backends.loading, func() []pane {
		b, ok := m.backends.list.selected()
		if !ok {
			return []pane{textPane(pathStyle.Render("(no backends)"))}
		}
		return []pane{m.backends.list.pane(m), textPane(""), textPane(m.backendDetail(b))}
	})
}

// -------------------------------------------------------------------------
// DETAIL
// -------------------------------------------------------------------------

// backendDetail renders one backend in full: its health and flags, where it
// is, what it holds, each of its limits as a bar against its ceiling, and the
// instance whose view of the breakers this is.
func (m *model) backendDetail(b *adminapi.BackendStatus) string {
	rows := []meterRow{storageRow("storage", b, "")}
	for _, u := range b.UsageLimits {
		rows = append(rows, usageMeterRow(output.MeterLabel(u.Kind, u.Budget), u, chargedCalls(u)))
	}
	return strings.Join([]string{
		colHeaderStyle.Render(b.Name) + "  " + backendMarkers(b),
		fieldBlock(summaryLabelWidth,
			field{"endpoint", profileText(b.Profile)},
			field{"holds", holdingsText(b)},
			field{"instance", output.Instance(m.backends.instance)},
		),
		m.meterBlock("usage   "+cycleText(b.UsageCycle), rows),
	}, "\n")
}

// profileText renders where a backend is and the largest object it takes,
// leaving out what its configuration does not set.
func profileText(p adminapi.BackendProfile) string {
	parts := []string{p.Endpoint}
	if p.Region != "" {
		parts = append(parts, "region "+p.Region)
	}
	parts = append(parts, "bucket "+p.Bucket)
	if p.MaxObjectSize > 0 {
		parts = append(parts, "max object "+humanize.Bytes(p.MaxObjectSize))
	}
	return strings.Join(parts, "   ")
}

// holdingsText renders what a backend holds: its objects, the copies backfill
// has yet to hash, the uploads in progress and, when compression saves
// anything there, how much.
func holdingsText(b *adminapi.BackendStatus) string {
	text := fmt.Sprintf("%s objects   %s unhashed   %s uploads in progress",
		humanize.Comma(b.ObjectCount),
		backlogStyle(b.UnhashedCopies).Render(humanize.Comma(b.UnhashedCopies)),
		humanize.Comma(b.ActiveMultipartUploads))
	if b.CompressionSavedBytes > 0 {
		text += "   " + statusOKStyle.Render(humanize.Bytes(b.CompressionSavedBytes)) + " saved"
	}
	return text
}

// chargedCallsShown is how many of a budget's calls the detail names before
// counting the rest, so a budget over every call stays on one line.
const chargedCallsShown = 3

// chargedCalls names the calls a request budget charges, in red once the
// budget is spent, since the backend then refuses them. A byte limit charges
// no named calls and renders nothing.
func chargedCalls(u adminapi.UsageMeter) string {
	if len(u.Operations) == 0 {
		return ""
	}
	shown := u.Operations[:min(len(u.Operations), chargedCallsShown)]
	names := make([]string, len(shown))
	for i, op := range shown {
		names[i] = string(op)
	}
	text := strings.Join(names, ", ")
	if rest := len(u.Operations) - len(shown); rest > 0 {
		text += fmt.Sprintf(" +%d more", rest)
	}
	style := pathStyle
	if u.Used >= u.Limit {
		style = statusErrStyle
	}
	return style.Render(text)
}

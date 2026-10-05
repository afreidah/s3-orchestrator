// -------------------------------------------------------------------------------
// TUI - Terminal Object Browser
//
// Author: Alex Freidah
//
// Bubble Tea program behind the `s3-orchestrator tui` subcommand. Loads one
// listing page from the admin API as an asynchronous command and renders it,
// tracking loading and error states through the Model / Update / View loop.
// -------------------------------------------------------------------------------

package tui

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	"github.com/afreidah/s3-orchestrator/internal/cli/admintarget"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// entry is one row in the listing: a child directory or a leaf object.
type entry struct {
	name  string
	isDir bool
	size  int64
}

// adminClient is the admin API surface the browser depends on. The concrete
// *apiClient satisfies it; tests inject a fake.
type adminClient interface {
	ListObjects(ctx context.Context, prefix, continuation string) (*adminapi.ObjectListResponse, error)
	GetObjectLocations(ctx context.Context, key string) (*adminapi.ObjectLocationsResponse, error)
	GetObjectTags(ctx context.Context, key string) (*adminapi.ObjectTagsResponse, error)
	ScrubKey(ctx context.Context, key string) (*adminapi.ScrubKeyResponse, error)
	GetStatus(ctx context.Context) (*adminapi.StatusResponse, error)
	GetLogs(ctx context.Context, level string) (*adminapi.LogsResponse, error)
	GetReplicationStatus(ctx context.Context) (*adminapi.ReplicationStatusResponse, error)
	GetWorkers(ctx context.Context) (*adminapi.WorkersResponse, error)
	GetCleanupQueue(ctx context.Context) (*adminapi.CleanupQueueResponse, error)
	GetCleanupDLQ(ctx context.Context) (*adminapi.CleanupDLQResponse, error)
	GetCacheStats(ctx context.Context) (*adminapi.CacheStatsResponse, error)
	GetProvisioning(ctx context.Context) (*adminapi.ProvisioningResponse, error)
	RequeueCleanupDLQ(ctx context.Context, backend string) (*adminapi.CleanupDLQRequeueResponse, error)
	RunOp(ctx context.Context, act *opsAction, req opsRequest) (adminclient.EventStream, error)
	ListObjectsFlat(ctx context.Context, prefix, continuation string) (*adminapi.ObjectListResponse, error)
	DownloadObject(ctx context.Context, key string) (io.ReadCloser, int64, error)
	UploadObject(ctx context.Context, key string, body io.Reader, size int64) error
	DeleteObject(ctx context.Context, key string) (*adminapi.ObjectDeleteResponse, error)
	DeletePrefix(ctx context.Context, prefix string) (*adminapi.ObjectDeleteResponse, error)
	StartDrain(ctx context.Context, backend string) (*adminapi.BackendOperationResponse, error)
	DrainProgress(ctx context.Context, backend string) (*adminapi.DrainProgressResponse, error)
	CancelDrain(ctx context.Context, backend string) (*adminapi.BackendOperationResponse, error)
	ReconcileBackend(ctx context.Context, backend string) (*adminapi.ReconcileResponse, error)
}

// model is the Bubble Tea state for the browser.
type model struct {
	client      adminClient
	section     section         // active left-nav destination (Files, Backends)
	navFocus    bool            // the left nav has focus and is capturing keys
	navCursor   int             // highlighted nav entry while the nav is focused
	mode        viewMode        // Files sub-state: the listing (browse) or the inspector
	insp        inspector       // inspector pane state, populated when mode is modeInspect
	backends    backendsView    // backends pane state, populated when section is sectionBackends
	buckets     bucketsView     // buckets pane state, populated when section is sectionBuckets
	logs        logsView        // logs pane state, populated when section is sectionLogs
	replication replicationView // replication pane state, populated when section is sectionReplication
	workers     workersView     // workers pane state, populated when section is sectionWorkers
	cleanup     cleanupView     // cleanup pane state, populated when section is sectionCleanup
	cache       cacheView       // cache pane state, populated when section is sectionCache
	ops         opsView         // ops pane state, populated when section is sectionOps
	run         run             // the one action streaming its output, shown by the pane that started it
	poll        poller          // when each pane's endpoint was last requested, and which are in flight
	files       fileAction      // the Files pane's in-flight transfer, if any
	prefix      string          // the prefix currently listed ("" is the root)
	entries     []entry         // every loaded row under the current prefix
	visible     []entry         // entries after filter + sort, indexed by table cursor
	table       table.Model     // scrolling, selectable listing table
	filter      textinput.Model // substring filter over the current listing
	filtering   bool            // the filter input has focus and is capturing keys
	sort        sortField       // ordering applied to visible
	loading     bool            // a fresh (page-replacing) load is in flight
	next        string          // continuation token for the current prefix ("" = no more)
	more        bool            // a load-more (append) request is in flight
	err         error           // last load error, if any
	spinner     spinner.Model   // animated indicator shown while loading
	confirm     *confirmPrompt  // armed confirmation for a pending write action, if any
	prompt      *inputPrompt    // armed input prompt for an action that needs a value, if any
	status      *actionStatus   // result of the last action, shown until the next keypress
	actionLog   []loggedAction  // every action result this session, oldest first
	help        bool            // the keymap overlay is showing in place of the pane
	dbHealthy   *bool           // metadata DB health from the last status fetch (nil = unknown)
	width       int             // terminal width from the last WindowSizeMsg
	height      int             // terminal height from the last WindowSizeMsg
}

// initialModel builds the starting state; loading is true because Init fires
// the first load immediately.
func initialModel(client adminClient) *model {
	m := &model{client: client, loading: true, spinner: spinner.New(), table: newTable(fileColumns), filter: newFilterInput()}
	m.logs = newLogsView()
	m.backends = newBackendsView()
	m.buckets = newBucketsView()
	m.workers = newWorkersView()
	m.cleanup = newCleanupView()
	return m
}

// reselect puts the cursor back on the row whose key is prev after the table's
// rows were replaced, so a refresh does not move the operator's selection.
// keys lists the new rows' keys in row order. A row that has gone keeps the
// cursor at the same position, clamped to the new last row.
func reselect(t *table.Model, keys []string, prev string) {
	idx := slices.Index(keys, prev)
	if idx < 0 {
		idx = min(t.Cursor(), len(keys)-1)
	}
	t.SetCursor(max(idx, 0))
}

// newFilterInput builds the substring filter input the Files and Logs panes
// share.
func newFilterInput() textinput.Model {
	fi := textinput.New()
	fi.Prompt = ""
	fi.Placeholder = "type to filter"
	return fi
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// objectsLoadedMsg carries a successfully loaded listing page. A non-empty
// continuation means this page appends to the current listing rather than
// replacing it.
type objectsLoadedMsg struct {
	prefix       string
	continuation string
	page         *adminapi.ObjectListResponse
}

// errMsg carries a failed load.
type errMsg struct{ err error }

// loadObjects returns a command that fetches one page under prefix off the main
// loop, delivering the result back as an objectsLoadedMsg or errMsg. A
// non-empty continuation resumes a truncated listing.
func (m *model) loadObjects(prefix, continuation string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		page, err := client.ListObjects(context.Background(), prefix, continuation)
		if err != nil {
			return errMsg{err}
		}
		return objectsLoadedMsg{prefix: prefix, continuation: continuation, page: page}
	}
}

// entriesFromPage flattens a listing page into rows, trimming the parent prefix
// so each row shows only its leaf name.
func entriesFromPage(prefix string, page *adminapi.ObjectListResponse) []entry {
	out := make([]entry, 0, len(page.CommonPrefixes)+len(page.Objects))
	for _, cp := range page.CommonPrefixes {
		out = append(out, entry{name: strings.TrimPrefix(cp, prefix), isDir: true})
	}
	for i := range page.Objects {
		out = append(out, entry{name: strings.TrimPrefix(page.Objects[i].Key, prefix), size: page.Objects[i].Size})
	}
	return out
}

// -------------------------------------------------------------------------
// BUBBLE TEA LOOP
// -------------------------------------------------------------------------

// Init fires the first load of the root prefix and starts the spinner and the
// poller ticking.
func (m *model) Init() tea.Cmd {
	// Fetch the dashboard's snapshots alongside the first listing, so the
	// section the TUI opens on, and the sidebar's DB-health indicator, fill
	// in at startup rather than on the poller's first tick.
	return tea.Batch(m.loadObjects(m.prefix, ""), m.refreshDashboard(), m.spinner.Tick, pollTick())
}

// Update handles one message and returns the next state. A result the poller
// was waiting on clears its in-flight mark first, whichever pane applies it.
// The backends pane's own messages are dispatched next, by the pane, so its
// drain bookkeeping lives with the code that reads it rather than swelling
// this switch.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if t, ok := pollTargetOf(msg); ok {
		m.poll.inFlight[t] = false
	}
	if model, cmd, handled := m.updateBackends(msg); handled {
		return model, cmd
	}
	if model, cmd, handled := m.updateFileActions(msg); handled {
		return model, cmd
	}

	switch msg := msg.(type) {
	case objectsLoadedMsg:
		m.applyPage(msg)
		return m, nil
	case errMsg:
		m.loading = false
		m.err = msg.err
		return m, nil
	case locationsLoadedMsg:
		m.applyLocations(msg.resp)
		return m, nil
	case locationsErrMsg:
		m.insp.loading = false
		m.insp.err = msg.err
		return m, nil
	case tagsLoadedMsg:
		m.applyTags(msg)
		return m, nil
	case scrubKeyMsg:
		return m.applyScrubKey(msg)
	case statusLoadedMsg:
		m.applyStatus(msg.resp)
		return m, nil
	case statusErrMsg:
		m.backends.loading = false
		m.backends.err = msg.err
		return m, nil
	case logsLoadedMsg:
		m.applyLogs(msg.resp)
		return m, nil
	case logsErrMsg:
		m.logs.loading = false
		m.logs.err = msg.err
		return m, nil
	case replicationLoadedMsg:
		m.applyReplication(msg.resp)
		return m, nil
	case replicationErrMsg:
		m.applyReplicationErr(msg.err)
		return m, nil
	case pollTickMsg:
		return m.onPollTick(msg.now)
	case workersLoadedMsg:
		m.applyWorkers(msg.resp)
		return m, nil
	case workersErrMsg:
		m.applyWorkersErr(msg.err)
		return m, nil
	case cleanupLoadedMsg:
		m.applyCleanup(msg)
		return m, nil
	case cleanupErrMsg:
		m.cleanup.loading = false
		m.cleanup.err = msg.err
		return m, nil
	case cleanupRequeuedMsg:
		return m.applyCleanupRequeued(msg)
	case cacheLoadedMsg:
		m.applyCache(msg.resp)
		return m, nil
	case cacheErrMsg:
		m.applyCacheErr(msg.err)
		return m, nil
	case bucketsLoadedMsg:
		m.applyBuckets(msg.resp)
		return m, nil
	case bucketsErrMsg:
		m.applyBucketsErr(msg.err)
		return m, nil
	case runStreamMsg:
		return m.applyRunStream(msg)
	case runEventMsg:
		return m.applyRunEvent(&msg.event)
	case runDoneMsg:
		return m.applyRunDone(msg)
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeLogs()
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleGlobalKey applies the keys that mean the same thing in every pane:
// quit, the nav toggle, help, and the single-letter section jumps. Reports
// whether the key was one of them, so the caller can route on to the active
// pane.
func (m *model) handleGlobalKey(key tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	switch key.String() {
	case "q", "ctrl+c":
		return m, tea.Quit, true
	case "tab":
		m.navFocus = !m.navFocus
		if m.navFocus {
			m.navCursor = int(m.section)
		}
		return m, nil, true
	case "?":
		m.help = true
		return m, nil, true
	}

	for _, s := range sectionKeys {
		if s.key == key.String() {
			model, cmd := m.selectSection(s.sec)
			return model, cmd, true
		}
	}
	return m, nil, false
}

// handleKey applies global keys (quit, nav focus, help, section jumps) then
// routes the rest to the focused nav or the active section's view. While the
// filter input is capturing, the browser gets every key so typing is never
// intercepted, and while the help is showing any key closes it.
func (m *model) handleKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	// An armed prompt captures the next key before anything else: the input
	// first, since typing a key or prefix must never reach the pane below.
	if m.prompt != nil {
		return m.handleInputKey(key)
	}
	if m.confirm != nil {
		return m.handleConfirmKey(key)
	}
	// Any keypress dismisses a lingering action-result line; the session log
	// keeps it.
	m.status = nil
	if m.help {
		m.help = false
		return m, nil
	}

	if m.section == sectionFiles && m.mode == modeBrowse && m.filtering {
		return m.handleFilterKey(key)
	}
	if m.section == sectionLogs && m.logs.filtering {
		return m.handleLogsFilterKey(key)
	}

	if model, cmd, handled := m.handleGlobalKey(key); handled {
		return model, cmd
	}

	if m.navFocus {
		return m.handleNavKey(key)
	}
	return m.handlePaneKey(key)
}

// handlePaneKey routes a key to the active section's pane, or to the run's
// output while that pane is showing it. Files has two panes, the listing and
// the inspector, chosen by mode.
func (m *model) handlePaneKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.showingRun() {
		return m.handleRunKey(key)
	}
	switch m.section {
	case sectionDashboard:
		return m.handleDashboardKey(key)
	case sectionLogs:
		return m.handleLogsKey(key)
	case sectionReplication:
		return m.handleReplicationKey(key)
	case sectionOps:
		return m.handleOpsKey(key)
	case sectionBackends:
		return m.handleBackendsKey(key)
	case sectionBuckets:
		return m.handleBucketsKey(key)
	case sectionWorkers:
		return m.handleWorkersKey(key)
	case sectionCleanup:
		return m.handleCleanupKey(key)
	case sectionCache:
		return m.handleCacheKey(key)
	}
	if m.mode == modeInspect {
		return m.handleInspectKey(key)
	}
	return m.handleBrowseKey(key)
}

// handleBrowseKey applies listing keys (filter, sort, navigate, reload) and
// delegates the rest (cursor movement, paging) to the table.
func (m *model) handleBrowseKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "/":
		m.filtering = true
		cmd := m.filter.Focus()
		return m, cmd
	case "s":
		m.sort = (m.sort + 1) % 2
		m.refreshVisible()
		return m, nil
	case "esc":
		if m.filter.Value() != "" {
			m.clearFilter()
			m.refreshVisible()
		}
		return m, nil
	case "enter", "right", "l":
		return m.open()
	case "backspace", "left", "h":
		return m.ascend()
	case "r":
		m.loading = true
		cmd := m.loadObjects(m.prefix, "")
		return m, cmd
	}

	if model, cmd, handled := m.handleFileActionKey(key.String()); handled {
		return model, cmd
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(key)
	if m.next != "" && !m.more && m.table.Cursor() >= len(m.visible)-1 {
		m.more = true
		return m, tea.Batch(cmd, m.loadObjects(m.prefix, m.next))
	}
	return m, cmd
}

// open acts on the highlighted row: a directory descends into its prefix, a
// leaf object opens the inspector on its full key.
func (m *model) open() (tea.Model, tea.Cmd) {
	idx := m.table.Cursor()
	if idx >= 0 && idx < len(m.visible) && !m.visible[idx].isDir {
		return m.openInspector(m.prefix + m.visible[idx].name)
	}
	return m.descend()
}

// applyPage folds a loaded page into the model: a continuation appends to the
// current listing, a fresh load replaces it. Either way it records the next
// continuation token so the bottom of the list can page further.
func (m *model) applyPage(msg objectsLoadedMsg) {
	loaded := entriesFromPage(msg.prefix, msg.page)
	if msg.continuation == "" {
		m.prefix = msg.prefix
		m.entries = loaded
		m.clearFilter() // the filter is prefix-specific; a new prefix starts fresh
		m.refreshVisible()
		m.table.SetCursor(0)
	} else {
		m.entries = append(m.entries, loaded...)
		m.refreshVisible()
	}
	m.next = ""
	if msg.page.Truncated {
		m.next = msg.page.Next
	}
	m.loading = false
	m.more = false
	m.err = nil
}

// descend loads the highlighted directory, if the selected row is one.
func (m *model) descend() (tea.Model, tea.Cmd) {
	idx := m.table.Cursor()
	if idx >= 0 && idx < len(m.visible) && m.visible[idx].isDir {
		m.loading = true
		cmd := m.loadObjects(m.prefix+m.visible[idx].name, "")
		return m, cmd
	}
	return m, nil
}

// ascend loads the parent prefix unless already at the root.
func (m *model) ascend() (tea.Model, tea.Cmd) {
	if m.prefix != "" {
		m.loading = true
		cmd := m.loadObjects(parentPrefix(m.prefix), "")
		return m, cmd
	}
	return m, nil
}

// fileColumns declares the listing table's columns. The name takes whatever
// width the type and size leave.
var fileColumns = []columnSpec{
	{title: "NAME", min: 10, max: 0, priority: 3},
	{title: "TYPE", min: 5, max: 5, priority: 1},
	{title: "SIZE", min: 12, max: 12, priority: 2},
}

// rowsFromEntries builds table rows from the domain entries, in the same order
// so the table cursor indexes straight into entries.
func rowsFromEntries(entries []entry) []table.Row {
	rows := make([]table.Row, 0, len(entries))
	for _, e := range entries {
		if e.isDir {
			rows = append(rows, table.Row{e.name, "dir", ""})
			continue
		}
		rows = append(rows, table.Row{e.name, "obj", humanize.Bytes(e.size)})
	}
	return rows
}

// parentPrefix returns the parent of a delimiter-terminated prefix, or "" when
// already at the root.
func parentPrefix(prefix string) string {
	p := strings.TrimSuffix(prefix, "/")
	if parent, _, ok := strings.CutLast(p, "/"); ok {
		return parent + "/"
	}
	return ""
}

// View composes the full-screen layout: a title bar on top, the body filling
// the available height, and a help bar pinned to the bottom.
func (m *model) View() string {
	if m.width == 0 {
		return "loading..."
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, m.sidebarView(), m.contentView())
}

// contentView renders the active section's pane for the area beside the nav.
func (m *model) contentView() string {
	if m.help {
		return m.helpPaneView()
	}
	if m.showingRun() {
		return m.runPaneView()
	}
	if m.section == sectionDashboard {
		return m.dashboardPaneView()
	}
	if m.section == sectionLogs {
		return m.logsPaneView()
	}
	if m.section == sectionReplication {
		return m.replicationPaneView()
	}
	if m.section == sectionOps {
		return m.opsPaneView()
	}
	if m.section == sectionBackends {
		return m.backendsPaneView()
	}
	if m.section == sectionBuckets {
		return m.bucketsPaneView()
	}
	if m.section == sectionWorkers {
		return m.workersPaneView()
	}
	if m.section == sectionCleanup {
		return m.cleanupPaneView()
	}
	if m.section == sectionCache {
		return m.cachePaneView()
	}
	if m.mode == modeInspect {
		return m.inspectView()
	}
	return m.frame(m.headerView(), m.footerView(), m.body()...)
}

// paneBody renders the three states every pane reports the same way - a load
// that failed, a pane this deployment did not wire, and a load still in flight
// - and defers to content for the pane's own panes. Panes whose data is
// always present pass an empty unavailable. Shared so the states a user reads
// as "something is wrong" cannot drift apart between panes.
func (m *model) paneBody(err error, unavailable string, loading bool, content func() []pane) []pane {
	switch {
	case err != nil:
		return []pane{textPane(errStyle.Render("error: " + err.Error()))}
	case unavailable != "":
		return []pane{textPane(pathStyle.Render("(" + unavailable + ")"))}
	case loading:
		return []pane{textPane(m.spinner.View() + " loading...")}
	default:
		return content()
	}
}

// headerView renders the full-width title bar with the current prefix.
// contentTitleStyle returns the title-bar style for the content pane: bright
// when the content has focus, muted while the nav has focus, so the focused
// pane reads at a glance.
func (m *model) contentTitleStyle() lipgloss.Style {
	if m.navFocus {
		return titleMutedStyle
	}
	return titleStyle
}

func (m *model) headerView() string {
	loc := m.prefix
	if loc == "" {
		loc = "/"
	}
	return m.contentTitleStyle().Width(m.contentWidth()).Render("s3-orchestrator tui   " + loc)
}

// footerView renders the status line (sort, filter, paging) above the key-hint
// bar. Both lines are always present so the footer keeps a fixed height.
func (m *model) footerView() string {
	status := m.statusLine()
	// A running transfer takes the status line: how far it has got matters
	// more than the match count while bytes are moving.
	if line := m.fileTransferLine(); line != "" {
		status = line
	}
	matches := pathStyle.Width(m.contentWidth()).Render(status)
	return lipgloss.JoinVertical(lipgloss.Left, matches, m.hintFooter())
}

// body renders the current content: an error, the loading indicator, an
// empty or no-matches notice, or the row list.
func (m *model) body() []pane {
	return m.paneBody(m.err, "", m.loading, func() []pane {
		switch {
		case len(m.entries) == 0:
			return []pane{textPane(pathStyle.Render("(empty)"))}
		case len(m.visible) == 0:
			return []pane{textPane(pathStyle.Render("(no matches)"))}
		default:
			return []pane{m.tablePane(&m.table, fileColumns)}
		}
	})
}

// -------------------------------------------------------------------------
// ENTRY POINT
// -------------------------------------------------------------------------

// target is the resolved admin endpoint and the keypair to reach it with.
type target struct {
	baseAddr    string
	accessKeyID string
	secretKey   string
}

// signs reports whether this target carries a keypair to sign with.
func (t target) signs() bool {
	return t.accessKeyID != "" && t.secretKey != ""
}

// parseArgs parses the tui flags into the theme to draw with and the admin
// target. The theme spec comes from -theme, else $S3O_TUI_THEME, and is
// checked first, so a bad one is reported whatever else is missing. The
// target's address and credential resolve flag -> env -> config; the address
// comes back http-prefixed, or an error says what is missing.
func parseArgs(args []string) (target, theme, error) {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "config.yaml", "Path to config file (only loaded when -addr/-token or their env vars are unset)")
	addr := fs.String("addr", "", "Server address (overrides $S3O_ADMIN_ADDR and config)")
	accessKey := fs.String("access-key", "", "Access key ID to sign with (overrides $S3O_ACCESS_KEY_ID)")
	secretKey := fs.String("secret-key", "", "Secret access key to sign with (overrides $S3O_SECRET_ACCESS_KEY)")
	themeSpec := fs.String("theme", "", "Colour theme: a preset, then slot:colour overrides (overrides $S3O_TUI_THEME)")
	if err := fs.Parse(args); err != nil {
		return target{}, theme{}, err
	}
	th, err := parseTheme(cmp.Or(*themeSpec, os.Getenv(EnvTheme)))
	if err != nil {
		return target{}, theme{}, err
	}
	t, err := resolveTarget(*configPath, *addr, *accessKey, *secretKey)
	return t, th, err
}

// resolveTarget resolves the admin base address and credential from the flag
// values, falling back to the environment and then the config file.
func resolveTarget(configPath, addr, accessKey, secretKey string) (target, error) {
	t := target{
		accessKeyID: cmp.Or(accessKey, os.Getenv(admintarget.EnvAccessKey)),
		secretKey:   cmp.Or(secretKey, os.Getenv(admintarget.EnvSecretKey)),
	}
	if !t.signs() {
		return target{}, errors.New("a credential is required (set -access-key and -secret-key, " +
			"or $S3O_ACCESS_KEY_ID and $S3O_SECRET_ACCESS_KEY)")
	}
	// The config file is only read when the address is still missing, so a
	// keypair and an address given outright need no config on this machine.
	baseAddr, err := admintarget.Resolve(addr, func() (*config.Config, error) {
		return config.LoadConfig(configPath)
	})
	if err != nil {
		return target{}, err
	}
	t.baseAddr = baseAddr
	if t.baseAddr == "" {
		return target{}, errors.New("admin address required (set -addr, $S3O_ADMIN_ADDR, or config)")
	}
	// A bare host:port defaults to http, because the common target is a local
	// instance reached over a loopback or a private network. An operator
	// pointing at a remote one supplies the scheme, and https is preserved
	// exactly because the prefix check passes it through untouched.
	if !strings.HasPrefix(t.baseAddr, "http") {
		t.baseAddr = "http://" + t.baseAddr //nolint:gosec // NOSONAR S5332: scheme default for an operator-supplied address
	}
	return t, nil
}

// Run resolves the admin target and theme, starts the TUI, and returns a
// process exit code.
func Run(args []string, _, stderr io.Writer) int { // codecov:ignore -- TUI entry point
	t, th, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	useTheme(&th)
	if _, err := tea.NewProgram(initialModel(newAPIClient(t)), tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintf(stderr, "tui error: %v\n", err)
		return 1
	}
	return 0
}

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
	"github.com/afreidah/s3-orchestrator/internal/cli/flagset"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	"github.com/afreidah/s3-orchestrator/internal/cli/admintarget"
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
	SetObjectTags(ctx context.Context, key string, tags []adminapi.ObjectTag) (*adminapi.ObjectTagsResponse, error)
	DeleteObjectTags(ctx context.Context, key string) (*adminapi.ObjectTagsResponse, error)
	ScrubKey(ctx context.Context, key string) (*adminapi.ScrubKeyResponse, error)
	GetStatus(ctx context.Context) (*adminapi.StatusResponse, error)
	GetLogs(ctx context.Context, level string) (*adminapi.LogsResponse, error)
	GetReplicationStatus(ctx context.Context) (*adminapi.ReplicationStatusResponse, error)
	GetWorkers(ctx context.Context) (*adminapi.WorkersResponse, error)
	GetConfig(ctx context.Context) (*adminapi.ConfigResponse, error)
	GetCleanupQueue(ctx context.Context) (*adminapi.CleanupQueueResponse, error)
	GetCleanupDLQ(ctx context.Context) (*adminapi.CleanupDLQResponse, error)
	GetCacheStats(ctx context.Context) (*adminapi.CacheStatsResponse, error)
	GetProvisioning(ctx context.Context) (*adminapi.ProvisioningResponse, error)
	RunOp(ctx context.Context, act *opsAction, req opsRequest) (adminclient.EventStream, error)
	ListObjectsFlat(ctx context.Context, prefix, continuation string) (*adminapi.ObjectListResponse, error)
	DownloadObject(ctx context.Context, key string) (io.ReadCloser, int64, error)
	UploadObject(ctx context.Context, key string, body io.Reader, size int64) error
	DeleteObject(ctx context.Context, key string) (*adminapi.ObjectDeleteResponse, error)
	DeletePrefix(ctx context.Context, prefix string) (*adminapi.ObjectDeleteResponse, error)
	StartDrain(ctx context.Context, backend string) (*adminapi.BackendOperationResponse, error)
	DrainProgress(ctx context.Context, backend string) (*adminapi.DrainProgressResponse, error)
	CancelDrain(ctx context.Context, backend string) (*adminapi.BackendOperationResponse, error)
	SetCordon(ctx context.Context, backend string, cordoned bool) (*adminapi.BackendOperationResponse, error)
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
	config      configView      // config pane state, populated when section is sectionConfig
	ops         opsView         // ops pane state, populated when section is sectionOps
	run         run             // the one action streaming its output, shown by the pane that started it
	poll        poller          // when each pane's endpoint was last requested, and which are in flight
	transfer    *transfer       // the Files pane's in-flight transfer, if any
	prefix      string          // the prefix currently listed ("" is the root)
	entries     []entry         // every loaded row under the current prefix
	visible     []entry         // entries after filter + sort, indexed by table cursor
	table       table.Model     // scrolling, selectable listing table
	filter      filterBox       // substring filter over the current listing
	sort        sortField       // ordering applied to visible
	loading     bool            // a fresh (page-replacing) load is in flight
	next        string          // continuation token for the current prefix ("" = no more)
	more        bool            // a load-more (append) request is in flight
	err         error           // last load error, if any
	spinner     spinner.Model   // animated indicator shown while loading
	confirm     *adminAction    // write action armed behind a y/N confirmation, if any
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
	m := &model{client: client, loading: true, spinner: spinner.New(), table: newTable(fileColumns), filter: newFilterBox()}
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

// filterBox is the substring filter the Files and Logs panes narrow their rows
// with. active is set while it has focus and captures keys.
type filterBox struct {
	textinput.Model
	active bool
}

// newFilterBox builds an empty, unfocused filter.
func newFilterBox() filterBox {
	fi := textinput.New()
	fi.Prompt = ""
	fi.Placeholder = "type to filter"
	return filterBox{Model: fi}
}

// open gives the filter focus, so keys edit it.
func (f *filterBox) open() tea.Cmd {
	f.active = true
	return f.Focus()
}

// clear empties the filter and takes its focus away.
func (f *filterBox) clear() {
	f.active = false
	f.Blur()
	f.SetValue("")
}

// handle feeds one key to the focused filter: esc clears it, enter keeps it
// applied and hands the keys back, and any other key edits it. onChange runs
// whenever the text changed, so the pane reshapes what it shows.
func (f *filterBox) handle(key tea.KeyMsg, onChange func()) tea.Cmd {
	switch key.String() {
	case "esc":
		f.clear()
		onChange()
		return nil
	case "enter":
		f.active = false
		f.Blur()
		return nil
	}
	var cmd tea.Cmd
	f.Model, cmd = f.Update(key)
	onChange()
	return cmd
}

// label renders the filter for a status or title line: the live input while
// it has focus, the applied text otherwise, and "" when there is none.
func (f *filterBox) label() string {
	if f.active {
		return f.View()
	}
	return f.Value()
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

// Update handles one message and returns the next state. A finished fetch folds
// itself in, so this switch never names a pane's response. The backends pane's
// own messages are dispatched first, by the pane, so its drain bookkeeping
// lives with the code that reads it rather than swelling this switch.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if model, cmd, handled := m.updateBackends(msg); handled {
		return model, cmd
	}
	if model, cmd, handled := m.updateFileActions(msg); handled {
		return model, cmd
	}

	switch msg := msg.(type) {
	case loadDone:
		m.poll.landed(msg.target)
		msg.apply(m)
		return m, nil
	case objectsLoadedMsg:
		m.applyPage(msg)
		return m, nil
	case errMsg:
		m.loading = false
		m.err = msg.err
		return m, nil
	case tagsLoadedMsg:
		m.applyTags(msg)
		return m, nil
	case scrubKeyMsg:
		return m.applyScrubKey(msg)
	case pollTickMsg:
		return m.onPollTick(msg.now)
	case actionDoneMsg:
		return m.applyActionDone(msg)
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

	for i, spec := range sectionSpecs() {
		if spec.jump == key.String() {
			model, cmd := m.selectSection(section(i))
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

	if m.section == sectionFiles && m.mode == modeBrowse && m.filter.active {
		return m, m.filter.handle(key, func() {
			m.refreshVisible()
			m.table.SetCursor(0)
		})
	}
	if m.section == sectionLogs && m.logs.filter.active {
		return m, m.logs.filter.handle(key, m.refreshLogLines)
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
// output while that pane is showing it. A pane that does not own esc and "r"
// goes back to the nav and reloads the same way as every other.
func (m *model) handlePaneKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.showingRun() {
		return m.handleRunKey(key)
	}
	spec := m.spec()
	if !spec.ownsBack {
		switch key.String() {
		case "esc", "left", "h":
			return m.navBack()
		case "r":
			cmd := m.reload(m.section)
			return m, cmd
		}
	}
	if spec.handle == nil {
		return m, nil
	}
	return spec.handle(m, key)
}

// handleFilesKey routes a key to the Files section's two panes, the listing and
// the inspector, chosen by mode.
func (m *model) handleFilesKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		return m, m.filter.open()
	case "s":
		m.sort = (m.sort + 1) % 2
		m.refreshVisible()
		return m, nil
	case "esc":
		if m.filter.Value() != "" {
			m.filter.clear()
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
		m.filter.clear() // the filter is prefix-specific; a new prefix starts fresh
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

// entryRow renders one listing entry.
func entryRow(e *entry) table.Row {
	if e.isDir {
		return table.Row{e.name, "dir", ""}
	}
	return table.Row{e.name, "obj", humanize.Bytes(e.size)}
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
	if spec := m.spec(); spec.header != nil {
		return m.frame(spec.header(m), m.hintFooter(), spec.body(m)...)
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
func (m *model) paneBody(s *loadState, content func() []pane) []pane {
	switch {
	case s.err != nil:
		return []pane{textPane(errStyle.Render("error: " + s.err.Error()))}
	case s.unavailable != "":
		return []pane{textPane(pathStyle.Render("(" + s.unavailable + ")"))}
	case s.loading:
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

// titleBar renders text as the content pane's full-width title bar.
func (m *model) titleBar(text string) string {
	return m.contentTitleStyle().Width(m.contentWidth()).Render(text)
}

func (m *model) headerView() string {
	loc := m.prefix
	if loc == "" {
		loc = "/"
	}
	return m.titleBar("s3-orchestrator tui   " + loc)
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
	return m.paneBody(&loadState{err: m.err, loading: m.loading}, func() []pane {
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

// parseArgs parses the tui flags into the theme to draw with and the admin
// target. The theme spec comes from -theme, else $S3O_TUI_THEME, and is
// checked first, so a bad one is reported whatever else is missing.
func parseArgs(args []string, stderr io.Writer) (admintarget.Target, theme, error) {
	fs := flagset.New("tui", io.Discard)
	targetFlags := admintarget.RegisterFlags(fs)
	themeSpec := fs.String("theme", "", "Colour theme: a preset, then slot:colour overrides (overrides $S3O_TUI_THEME)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stderr)
			fs.Usage()
		}
		return admintarget.Target{}, theme{}, err
	}
	th, err := parseTheme(cmp.Or(*themeSpec, os.Getenv(EnvTheme)))
	if err != nil {
		return admintarget.Target{}, theme{}, err
	}
	t, err := targetFlags.Resolve()
	return t, th, err
}

// Run resolves the admin target and theme, starts the TUI, and returns a
// process exit code.
func Run(args []string, _, stderr io.Writer) int { // codecov:ignore -- TUI entry point
	t, th, err := parseArgs(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
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

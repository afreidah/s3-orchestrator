// -------------------------------------------------------------------------------
// TUI - Terminal Object Browser
//
// Author: Alex Freidah
//
// Bubble Tea program behind the `s3-orchestrator tui` subcommand. Loads one
// listing page from the admin API as an asynchronous command and renders it,
// tracking loading and error states through the Model / Update / View loop.
// -------------------------------------------------------------------------------

// Package tui implements the `s3-orchestrator tui` terminal browser.
package tui

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

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
	GetStatus(ctx context.Context) (*adminapi.StatusResponse, error)
	GetLogs(ctx context.Context, level string) (*adminapi.LogsResponse, error)
	GetReplicationStatus(ctx context.Context) (*adminapi.ReplicationStatusResponse, error)
	RunOp(ctx context.Context, act opsAction) (eventStream, error)
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
	logs        logsView        // logs pane state, populated when section is sectionLogs
	replication replicationView // replication pane state, populated when section is sectionReplication
	ops         opsView         // ops pane state, populated when section is sectionOps
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
	status      *actionStatus   // result of the last action, shown until the next keypress
	dbHealthy   *bool           // metadata DB health from the last status fetch (nil = unknown)
	width       int             // terminal width from the last WindowSizeMsg
	height      int             // terminal height from the last WindowSizeMsg
}

// initialModel builds the starting state; loading is true because Init fires
// the first load immediately.
func initialModel(client adminClient) *model {
	fi := textinput.New()
	fi.Prompt = ""
	fi.Placeholder = "type to filter"
	m := &model{client: client, loading: true, spinner: spinner.New(), table: newTable(), filter: fi}
	m.backends = backendsView{table: newTable()}
	// Seed the browser and backends columns up front. The initial loads can be
	// delivered before the first WindowSizeMsg, and SetRows on a column-less
	// table panics in the table's row renderer.
	m.resizeTable()
	m.resizeBackends()
	return m
}

// newTable builds a focused table styled to match the browser: a bold accent
// header and the shared selected-row highlight.
func newTable() table.Model {
	t := table.New(table.WithFocused(true))
	st := table.DefaultStyles()
	st.Header = st.Header.Bold(true).Foreground(lipgloss.Color("39"))
	st.Selected = selectedStyle
	t.SetStyles(st)
	return t
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

// Init fires the first load of the root prefix and starts the spinner ticking.
func (m *model) Init() tea.Cmd {
	// Fetch status alongside the first listing so the sidebar's DB-health
	// indicator is populated from startup, on any section.
	return tea.Batch(m.loadObjects(m.prefix, ""), m.loadStatus(), m.spinner.Tick)
}

// Update handles one message and returns the next state.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
	case replicationTickMsg:
		return m.onReplicationTick()
	case opsStreamMsg:
		return m.applyOpsStream(msg)
	case opsEventMsg:
		return m.applyOpsEvent(&msg.event)
	case opsDoneMsg:
		return m.applyOpsDone(msg)
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeTable()
		m.resizeInspector()
		m.resizeBackends()
		m.resizeLogs()
		m.resizeOps()
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey applies global keys (quit, nav focus, section jumps) then routes the
// rest to the focused nav or the active section's view. While the filter input
// is capturing, the browser gets every key so typing is never intercepted.
func (m *model) handleKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A pending confirmation captures the next key before anything else.
	if m.confirm != nil {
		return m.handleConfirmKey(key)
	}
	// Any keypress dismisses a lingering action-result line.
	m.status = nil

	if m.section == sectionFiles && m.mode == modeBrowse && m.filtering {
		return m.handleFilterKey(key)
	}

	switch key.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		m.navFocus = !m.navFocus
		if m.navFocus {
			m.navCursor = int(m.section)
		}
		return m, nil
	case "f":
		return m.selectSection(sectionFiles)
	case "b":
		return m.selectSection(sectionBackends)
	case "p":
		return m.selectSection(sectionReplication)
	case "l":
		return m.selectSection(sectionLogs)
	case "o":
		return m.selectSection(sectionOps)
	}

	if m.navFocus {
		return m.handleNavKey(key)
	}
	if m.section == sectionLogs {
		return m.handleLogsKey(key)
	}
	if m.section == sectionReplication {
		return m.handleReplicationKey(key)
	}
	if m.section == sectionOps {
		return m.handleOpsKey(key)
	}
	if m.section == sectionBackends {
		return m.handleBackendsKey(key)
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

// resizeTable fits the table columns and viewport to the current window. The
// height reserves one title row plus the two-line footer (hints + status).
func (m *model) resizeTable() {
	cw := m.contentWidth()
	nameWidth := max(cw-24, 10)
	m.table.SetColumns([]table.Column{
		{Title: "NAME", Width: nameWidth},
		{Title: "TYPE", Width: 5},
		{Title: "SIZE", Width: 12},
	})
	m.table.SetWidth(cw)
	m.table.SetHeight(max(m.height-3, 3))
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
		rows = append(rows, table.Row{e.name, "obj", humanSize(e.size)})
	}
	return rows
}

// humanSize renders a byte count in IEC binary units (B, KiB, MiB, ...), with
// one decimal place above bytes so sizes read at a glance.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// parentPrefix returns the parent of a delimiter-terminated prefix, or "" when
// already at the root.
func parentPrefix(prefix string) string {
	p := strings.TrimSuffix(prefix, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i+1]
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
	if m.mode == modeInspect {
		return m.inspectView()
	}
	return m.frame(m.headerView(), m.footerView(), m.bodyView())
}

// frame stacks a header, a body sized to fill the remaining height, and a
// footer into the full-screen layout shared by the browser and the inspector.
func (m *model) frame(header, footer, body string) string {
	bodyHeight := max(m.height-lipgloss.Height(header)-lipgloss.Height(footer), 1)
	rendered := lipgloss.NewStyle().Width(m.contentWidth()).Height(bodyHeight).MaxHeight(bodyHeight).Render(body)
	return lipgloss.JoinVertical(lipgloss.Left, header, rendered, footer)
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
	matches := pathStyle.Width(m.contentWidth()).Render(m.statusLine())
	hints := m.footer("up/down move - enter open - / filter - s sort - tab nav - q quit")
	return lipgloss.JoinVertical(lipgloss.Left, matches, hints)
}

// bodyView renders the current content: an error, the loading indicator, an
// empty or no-matches notice, or the row list.
func (m *model) bodyView() string {
	switch {
	case m.err != nil:
		return errStyle.Render("error: " + m.err.Error())
	case m.loading:
		return m.spinner.View() + " loading..."
	case len(m.entries) == 0:
		return pathStyle.Render("(empty)")
	case len(m.visible) == 0:
		return pathStyle.Render("(no matches)")
	default:
		return m.table.View()
	}
}

// -------------------------------------------------------------------------
// ENTRY POINT
// -------------------------------------------------------------------------

// resolveTarget parses the tui flags and resolves the admin base address and
// token (flag -> env -> config), returning an http-prefixed base address or an
// error describing what is missing.
func resolveTarget(args []string) (baseAddr, token string, err error) {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "config.yaml", "Path to config file (only loaded when -addr/-token or their env vars are unset)")
	addr := fs.String("addr", "", "Server address (overrides $S3O_ADMIN_ADDR and config)")
	tokenFlag := fs.String("token", "", "Admin API token (overrides $S3O_ADMIN_TOKEN and config)")
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}

	baseAddr, token, err = admintarget.Resolve(*addr, *tokenFlag, func() (*config.Config, error) {
		return config.LoadConfig(*configPath)
	})
	if err != nil {
		return "", "", err
	}
	if baseAddr == "" || token == "" {
		return "", "", errors.New("admin address and token required (set -addr/-token, $S3O_ADMIN_ADDR/$S3O_ADMIN_TOKEN, or config)")
	}
	if !strings.HasPrefix(baseAddr, "http") {
		baseAddr = "http://" + baseAddr
	}
	return baseAddr, token, nil
}

// Run resolves the admin target, starts the TUI, and returns a process exit
// code.
func Run(args []string, _, stderr io.Writer) int { // codecov:ignore -- TUI entry point
	baseAddr, token, err := resolveTarget(args)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if _, err := tea.NewProgram(initialModel(newAPIClient(baseAddr, token)), tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintf(stderr, "tui error: %v\n", err)
		return 1
	}
	return 0
}

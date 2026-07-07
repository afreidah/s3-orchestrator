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
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// entry is one row in the listing: a child directory or a leaf object.
type entry struct {
	name  string
	isDir bool
	size  int64
}

// objectLister is the admin API surface the browser depends on. The concrete
// *apiClient satisfies it; tests inject a fake.
type objectLister interface {
	ListObjects(ctx context.Context, prefix, continuation string) (*adminapi.ObjectListResponse, error)
}

// model is the Bubble Tea state for the browser.
type model struct {
	client  objectLister
	prefix  string        // the prefix currently listed ("" is the root)
	entries []entry       // domain rows backing the table, indexed by table cursor
	table   table.Model   // scrolling, selectable listing table
	loading bool          // a fresh (page-replacing) load is in flight
	next    string        // continuation token for the current prefix ("" = no more)
	more    bool          // a load-more (append) request is in flight
	err     error         // last load error, if any
	spinner spinner.Model // animated indicator shown while loading
	width   int           // terminal width from the last WindowSizeMsg
	height  int           // terminal height from the last WindowSizeMsg
}

// initialModel builds the starting state; loading is true because Init fires
// the first load immediately.
func initialModel(client objectLister) model {
	t := table.New(table.WithFocused(true))
	st := table.DefaultStyles()
	st.Header = st.Header.Bold(true).Foreground(lipgloss.Color("39"))
	st.Selected = selectedStyle
	t.SetStyles(st)
	return model{client: client, loading: true, spinner: spinner.New(), table: t}
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
func (m model) loadObjects(prefix, continuation string) tea.Cmd {
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
func (m model) Init() tea.Cmd {
	return tea.Batch(m.loadObjects(m.prefix, ""), m.spinner.Tick)
}

// Update handles one message and returns the next state.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case objectsLoadedMsg:
		m.applyPage(msg)
		return m, nil
	case errMsg:
		m.loading = false
		m.err = msg.err
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeTable()
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey applies application-level keys (quit, navigate, reload) and
// delegates everything else (cursor movement, paging) to the table.
func (m model) handleKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "enter", "right", "l":
		return m.descend()
	case "backspace", "left", "h":
		return m.ascend()
	case "r":
		m.loading = true
		return m, m.loadObjects(m.prefix, "")
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(key)
	if m.next != "" && !m.more && m.table.Cursor() >= len(m.entries)-1 {
		m.more = true
		return m, tea.Batch(cmd, m.loadObjects(m.prefix, m.next))
	}
	return m, cmd
}

// applyPage folds a loaded page into the model: a continuation appends to the
// current listing, a fresh load replaces it. Either way it records the next
// continuation token so the bottom of the list can page further.
func (m *model) applyPage(msg objectsLoadedMsg) {
	loaded := entriesFromPage(msg.prefix, msg.page)
	if msg.continuation == "" {
		m.prefix = msg.prefix
		m.entries = loaded
		m.table.SetRows(rowsFromEntries(m.entries))
		m.table.SetCursor(0)
	} else {
		m.entries = append(m.entries, loaded...)
		m.table.SetRows(rowsFromEntries(m.entries))
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
func (m model) descend() (tea.Model, tea.Cmd) {
	idx := m.table.Cursor()
	if idx >= 0 && idx < len(m.entries) && m.entries[idx].isDir {
		m.loading = true
		return m, m.loadObjects(m.prefix+m.entries[idx].name, "")
	}
	return m, nil
}

// ascend loads the parent prefix unless already at the root.
func (m model) ascend() (tea.Model, tea.Cmd) {
	if m.prefix != "" {
		m.loading = true
		return m, m.loadObjects(parentPrefix(m.prefix), "")
	}
	return m, nil
}

// resizeTable fits the table columns and viewport to the current window.
func (m *model) resizeTable() {
	nameWidth := max(m.width-24, 10)
	m.table.SetColumns([]table.Column{
		{Title: "NAME", Width: nameWidth},
		{Title: "TYPE", Width: 5},
		{Title: "SIZE", Width: 12},
	})
	m.table.SetWidth(m.width)
	m.table.SetHeight(max(m.height-2, 3))
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
		rows = append(rows, table.Row{e.name, "obj", strconv.FormatInt(e.size, 10)})
	}
	return rows
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
func (m model) View() string {
	if m.width == 0 {
		return "loading..."
	}

	header := m.headerView()
	footer := m.footerView()
	bodyHeight := max(m.height-lipgloss.Height(header)-lipgloss.Height(footer), 1)
	body := lipgloss.NewStyle().Width(m.width).Height(bodyHeight).MaxHeight(bodyHeight).Render(m.bodyView())

	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// headerView renders the full-width title bar with the current prefix.
func (m model) headerView() string {
	loc := m.prefix
	if loc == "" {
		loc = "/"
	}
	return titleStyle.Width(m.width).Render("s3-orchestrator tui   " + loc)
}

// footerView renders the full-width key-hint bar, noting when more pages remain.
func (m model) footerView() string {
	hints := "up/down move - enter open - backspace up - r reload - q quit"
	if m.next != "" {
		hints += " - (more below)"
	}
	return helpStyle.Width(m.width).Render(hints)
}

// bodyView renders the current content: an error, the loading indicator, an
// empty notice, or the row list.
func (m model) bodyView() string {
	switch {
	case m.err != nil:
		return errStyle.Render("error: " + m.err.Error())
	case m.loading:
		return m.spinner.View() + " loading..."
	case len(m.entries) == 0:
		return pathStyle.Render("(empty)")
	default:
		return m.table.View()
	}
}

// -------------------------------------------------------------------------
// ENTRY POINT
// -------------------------------------------------------------------------

// Run resolves the admin target, starts the TUI, and returns a process exit
// code.
func Run(args []string, _, stderr io.Writer) int { // codecov:ignore -- TUI entry point
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "Path to config file (only loaded when -addr/-token or their env vars are unset)")
	addr := fs.String("addr", "", "Server address (overrides $S3O_ADMIN_ADDR and config)")
	tokenFlag := fs.String("token", "", "Admin API token (overrides $S3O_ADMIN_TOKEN and config)")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	baseAddr, token, err := admintarget.Resolve(*addr, *tokenFlag, func() (*config.Config, error) {
		return config.LoadConfig(*configPath)
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if baseAddr == "" || token == "" {
		fmt.Fprintln(stderr, "error: admin address and token required (set -addr/-token, $S3O_ADMIN_ADDR/$S3O_ADMIN_TOKEN, or config)")
		return 1
	}
	if !strings.HasPrefix(baseAddr, "http") {
		baseAddr = "http://" + baseAddr
	}

	if _, err := tea.NewProgram(initialModel(newAPIClient(baseAddr, token)), tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintf(stderr, "tui error: %v\n", err)
		return 1
	}
	return 0
}

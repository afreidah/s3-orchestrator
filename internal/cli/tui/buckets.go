// -------------------------------------------------------------------------------
// TUI - Buckets View
//
// Author: Alex Freidah
//
// Read-only pane over the virtual buckets a deployment declares, from the
// config file and the store together. The pane exists to answer one question an
// operator cannot answer from the config file alone: which buckets exist, and
// which of them that file is authoritative for. Entries marked "config" are the
// ones the provisioning API refuses to change.
//
// Provisioning happens through the CLI, so nothing here writes. Reached with
// "v"; "esc" returns focus to the nav, "r" reloads.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// bucketsView holds the state of the buckets pane.
type bucketsView struct {
	rows        []adminapi.Bucket // one entry per declared bucket, both sources
	users       []adminapi.User   // the identities that reach them, for the grant column
	notices     []adminapi.Notice // what the merge of the two sources found
	table       table.Model       // scrolling table over the buckets
	loading     bool              // a fetch is in flight
	unavailable string            // set when the endpoint reports the feature is not wired
	err         error             // last fetch error, if any
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// bucketsLoadedMsg carries a successfully loaded provisioning snapshot.
type bucketsLoadedMsg struct {
	resp *adminapi.ProvisioningResponse
}

// bucketsErrMsg carries a failed provisioning fetch.
type bucketsErrMsg struct{ err error }

// loadBuckets returns a command that fetches the provisioning snapshot off the
// main loop.
func (m *model) loadBuckets() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.GetProvisioning(context.Background())
		if err != nil {
			return bucketsErrMsg{err}
		}
		return bucketsLoadedMsg{resp}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyBuckets folds a loaded snapshot into the pane state.
func (m *model) applyBuckets(resp *adminapi.ProvisioningResponse) {
	m.buckets.rows = resp.Buckets
	m.buckets.users = resp.Users
	m.buckets.notices = resp.Notices
	m.buckets.table.SetRows(rowsFromBuckets(resp.Buckets, resp.Users))
	m.buckets.table.SetCursor(0)
	m.buckets.loading = false
	m.buckets.unavailable = ""
	m.buckets.err = nil
}

// applyBucketsErr records a failed fetch, separating a deployment whose store
// half is not wired from a real failure.
func (m *model) applyBucketsErr(err error) {
	m.buckets.loading = false
	m.buckets.unavailable = adminclient.UnavailableReason(err)
	m.buckets.err = nil
	if m.buckets.unavailable == "" {
		m.buckets.err = err
	}
}

// handleBucketsKey applies pane keys (back, reload) and delegates cursor
// movement to the table.
func (m *model) handleBucketsKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "left", "h":
		return m.navBack()
	case "r":
		m.buckets.loading = true
		cmd := m.loadBuckets()
		return m, cmd
	}

	var cmd tea.Cmd
	m.buckets.table, cmd = m.buckets.table.Update(key)
	return m, cmd
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// resizeBuckets fits the buckets columns and viewport to the window.
func (m *model) resizeBuckets() {
	const (
		fixed   = 9 + 6 // multipart, source
		cols    = 4
		nameCap = 32
	)
	nameWidth := fitFirstColumn(m.contentWidth(), fixed, cols, nameCap)
	usersWidth := max(m.contentWidth()-nameWidth-fixed-cols*tableCellPad, 8)
	m.buckets.table.SetColumns([]table.Column{
		{Title: "BUCKET", Width: nameWidth},
		{Title: "MULTIPART", Width: 9},
		{Title: "SOURCE", Width: 6},
		{Title: "REACHED BY", Width: usersWidth},
	})
	m.buckets.table.SetWidth(m.contentWidth())
	m.buckets.table.SetHeight(max(m.height-3, 3))
}

// rowsFromBuckets builds table rows in the order the response listed them, so
// the table cursor indexes straight into rows.
func rowsFromBuckets(buckets []adminapi.Bucket, users []adminapi.User) []table.Row {
	rows := make([]table.Row, 0, len(buckets))
	for i := range buckets {
		b := buckets[i]
		rows = append(rows, table.Row{
			b.Name,
			multipartCap(b.MaxMultipartUploads),
			b.Source,
			strings.Join(usersReaching(users, b.Name), ", "),
		})
	}
	return rows
}

// multipartCap renders a bucket's multipart limit, spelling out that zero means
// no cap rather than no uploads.
func multipartCap(n int) string {
	if n == 0 {
		return "unlimited"
	}
	return strconv.Itoa(n)
}

// usersReaching names the identities holding a grant on a bucket and what each
// grant carries, so an operator sees at a glance which buckets nothing can
// reach yet and which are reached read-only.
//
// Falls back to the name alone when the server sent no grants, so the pane
// still renders against an older instance.
func usersReaching(users []adminapi.User, bucket string) []string {
	var out []string
	for i := range users {
		u := &users[i]
		if perms, ok := grantOn(u, bucket); ok {
			out = append(out, u.Name+perms)
			continue
		}
		for _, b := range u.Buckets {
			if b == bucket {
				out = append(out, u.Name)
				break
			}
		}
	}
	return out
}

// grantOn reports the rendered permissions a user's grant on a bucket carries.
func grantOn(u *adminapi.User, bucket string) (string, bool) {
	for _, g := range u.Grants {
		if g.Bucket == bucket {
			return "(" + strings.Join(g.Permissions, ",") + ")", true
		}
	}
	return "", false
}

// bucketsPaneView composes the pane's full-screen layout.
func (m *model) bucketsPaneView() string {
	return m.frame(m.bucketsHeaderView(), m.bucketsFooterView(), m.bucketsBodyView())
}

// bucketsHeaderView renders the title bar with the bucket count and how many
// the config file declares, since those are the read-only ones.
func (m *model) bucketsHeaderView() string {
	title := fmt.Sprintf("buckets   %d declared", len(m.buckets.rows))
	if n := configBuckets(m.buckets.rows); n > 0 {
		title += fmt.Sprintf("   %d from config (read-only)", n)
	}
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title)
}

// configBuckets counts the entries the config file declares.
func configBuckets(buckets []adminapi.Bucket) int {
	n := 0
	for i := range buckets {
		if buckets[i].Source == adminapi.SourceConfig {
			n++
		}
	}
	return n
}

// bucketsFooterView renders the buckets key hints. Nothing here writes:
// provisioning happens through the admin CLI.
func (m *model) bucketsFooterView() string {
	return m.footer("up/down move - r reload - tab nav - q quit")
}

// bucketsBodyView renders the current content: an error, a not-wired notice,
// the loading indicator, or the buckets table with anything the merge reported.
func (m *model) bucketsBodyView() string {
	return m.paneBody(m.buckets.err, m.buckets.unavailable, m.buckets.loading, func() string {
		if len(m.buckets.rows) == 0 {
			return pathStyle.Render("(no buckets declared)")
		}
		return m.buckets.table.View() + m.bucketNoticesView()
	})
}

// bucketNoticesView renders what the merge of the two sources found worth
// reporting, so a dangling grant is visible without reading the server log.
func (m *model) bucketNoticesView() string {
	if len(m.buckets.notices) == 0 {
		return ""
	}
	var b strings.Builder
	for _, n := range m.buckets.notices {
		fmt.Fprintf(&b, "\n%s %s", statusErrStyle.Render(n.Kind), pathStyle.Render(n.Detail))
	}
	return b.String()
}

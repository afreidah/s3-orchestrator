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
	"math"
	"strconv"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// bucketsView holds the state of the buckets pane.
type bucketsView struct {
	list        sortTable[bucketEntry] // one entry per declared bucket, both sources
	notices     []adminapi.Notice      // what the merge of the two sources found
	loading     bool                   // a fetch is in flight
	unavailable string                 // set when the endpoint reports the feature is not wired
	err         error                  // last fetch error, if any
}

// bucketEntry is one bucket with the identities that reach it, resolved
// against the users in the same snapshot when it loads.
type bucketEntry struct {
	adminapi.Bucket
	reachedBy []string
}

// newBucketsView builds the pane's empty state.
func newBucketsView() bucketsView {
	return bucketsView{list: newSortTable(bucketColumns, bucketSorts, rowsFromBuckets,
		func(b *bucketEntry) string { return b.Name })}
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
	entries := make([]bucketEntry, len(resp.Buckets))
	for i := range resp.Buckets {
		entries[i] = bucketEntry{Bucket: resp.Buckets[i], reachedBy: usersReaching(resp.Users, resp.Buckets[i].Name)}
	}
	m.buckets.list.setItems(entries)
	m.buckets.notices = resp.Notices
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
		m.buckets.loading = !m.buckets.list.loaded()
		cmd := m.fetch(pollBuckets)
		return m, cmd
	}

	cmd := m.buckets.list.update(key)
	return m, cmd
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// bucketColumns declares the buckets table's columns. The name is capped and
// the grants take the rest, since a long list of identities is what needs the
// width.
var bucketColumns = []columnSpec{
	{title: "BUCKET", min: 8, max: 32, priority: 4},
	{title: "MULTIPART", min: 9, max: 9, priority: 1},
	{title: "SOURCE", min: 6, max: 6, priority: 2},
	{title: "REACHED BY", min: 8, max: 0, priority: 3},
}

// bucketSorts orders the buckets table by every column. A multipart cap of 0
// means unlimited, so it sorts above every real cap; REACHED BY sorts by how
// many identities reach the bucket.
var bucketSorts = map[string]func(a, b *bucketEntry) int{
	"BUCKET":     by(func(b *bucketEntry) string { return b.Name }),
	"MULTIPART":  by(func(b *bucketEntry) int { return multipartRank(b.MaxMultipartUploads) }),
	"SOURCE":     by(func(b *bucketEntry) string { return b.Source }),
	"REACHED BY": by(func(b *bucketEntry) int { return len(b.reachedBy) }),
}

// multipartRank orders a multipart cap with 0, which means unlimited, above
// every real cap.
func multipartRank(n int) int {
	if n == 0 {
		return math.MaxInt
	}
	return n
}

// rowsFromBuckets builds table rows in display order, so the table cursor
// indexes straight into the entries.
func rowsFromBuckets(buckets []bucketEntry) []table.Row {
	rows := make([]table.Row, 0, len(buckets))
	for i := range buckets {
		b := &buckets[i]
		rows = append(rows, table.Row{
			b.Name,
			multipartCap(b.MaxMultipartUploads),
			b.Source,
			strings.Join(b.reachedBy, ", "),
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
// grant carries. It falls back to the name alone when the server sent no
// grants (an older instance).
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
		if g.Kind == string(core.ResourceBucket) && g.Name == bucket {
			return "(" + strings.Join(g.Permissions, ",") + ")", true
		}
	}
	return "", false
}

// bucketsPaneView composes the pane's full-screen layout.
func (m *model) bucketsPaneView() string {
	return m.frame(m.bucketsHeaderView(), m.hintFooter(), m.bucketsBody()...)
}

// bucketsHeaderView renders the title bar with the bucket count and how many
// the config file declares, since those are the read-only ones.
func (m *model) bucketsHeaderView() string {
	title := fmt.Sprintf("buckets   %d declared", len(m.buckets.list.received))
	if n := configBuckets(m.buckets.list.received); n > 0 {
		title += fmt.Sprintf("   %d from config (read-only)", n)
	}
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title)
}

// configBuckets counts the entries the config file declares.
func configBuckets(buckets []bucketEntry) int {
	n := 0
	for i := range buckets {
		if buckets[i].Source == adminapi.SourceConfig {
			n++
		}
	}
	return n
}

// bucketsBody renders the current content: an error, a not-wired notice, the
// loading indicator, or the buckets table with anything the merge reported
// below it.
func (m *model) bucketsBody() []pane {
	return m.paneBody(m.buckets.err, m.buckets.unavailable, m.buckets.loading, func() []pane {
		if len(m.buckets.list.received) == 0 {
			return []pane{textPane(pathStyle.Render("(no buckets declared)"))}
		}
		body := []pane{m.buckets.list.pane(m)}
		if len(m.buckets.notices) > 0 {
			body = append(body, textPane(m.bucketNoticesView()))
		}
		return body
	})
}

// bucketNoticesView renders what the merge of the two sources found worth
// reporting, so a dangling grant is visible without reading the server log.
func (m *model) bucketNoticesView() string {
	lines := make([]string, len(m.buckets.notices))
	for i, n := range m.buckets.notices {
		lines[i] = statusErrStyle.Render(n.Kind) + " " + pathStyle.Render(n.Detail)
	}
	return strings.Join(lines, "\n")
}

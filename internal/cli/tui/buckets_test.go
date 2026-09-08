// -------------------------------------------------------------------------------
// TUI - Buckets View Tests
//
// Author: Alex Freidah
//
// Deterministic tests for the buckets pane: the table marking which source
// declared each entry, the identities reaching a bucket, the header's read-only
// count, body rendering per state, key handling, and the split between a
// deployment whose store half is not wired and a real fetch failure.
// -------------------------------------------------------------------------------

package tui

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// bucketSnapshot is a merged view carrying one entry from each source, with one
// identity reaching the stored bucket.
func bucketSnapshot() *adminapi.ProvisioningResponse {
	return &adminapi.ProvisioningResponse{
		Buckets: []adminapi.Bucket{
			{Name: "from-config", Source: adminapi.SourceConfig},
			{Name: "from-store", MaxMultipartUploads: 4, Source: adminapi.SourceStore},
		},
		Users: []adminapi.User{
			{ID: "user-abc", Name: "ci", Buckets: []string{"from-store"}, Source: adminapi.SourceStore},
		},
	}
}

// bucketsModel builds a sized model on the buckets section with a snapshot
// already applied.
func bucketsModel(t *testing.T) *model {
	t.Helper()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.section = sectionBuckets
	m.resizeBuckets()
	m.applyBuckets(bucketSnapshot())
	return m
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// TestBucketsTable_MarksTheSource is what the pane exists for: an operator can
// see which buckets the config file declares, which are the ones nothing can
// change through the API.
func TestBucketsTable_MarksTheSource(t *testing.T) {
	t.Parallel()
	m := bucketsModel(t)

	got := m.buckets.table.View()
	for _, want := range []string{"from-config", "config", "from-store", "store"} {
		if !strings.Contains(got, want) {
			t.Errorf("table missing %q:\n%s", want, got)
		}
	}
}

// TestBucketsTable_ShowsWhoReachesEach verifies the grant column names the
// identities holding a grant, so a bucket nothing can reach is visible as such.
func TestBucketsTable_ShowsWhoReachesEach(t *testing.T) {
	t.Parallel()
	m := bucketsModel(t)

	rows := rowsFromBuckets(m.buckets.rows, m.buckets.users)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0][3] != "" {
		t.Errorf("from-config reached by %q, want nothing", rows[0][3])
	}
	if rows[1][3] != "ci" {
		t.Errorf("from-store reached by %q, want ci", rows[1][3])
	}
}

// TestBucketsTable_MultipartCap verifies zero renders as unlimited rather than
// as a bucket that accepts no multipart uploads.
func TestBucketsTable_MultipartCap(t *testing.T) {
	t.Parallel()

	if got := multipartCap(0); got != "unlimited" {
		t.Errorf("multipartCap(0) = %q, want unlimited", got)
	}
	if got := multipartCap(4); got != "4" {
		t.Errorf("multipartCap(4) = %q, want 4", got)
	}
}

// TestBucketsHeader_CountsReadOnly verifies the header says how many entries the
// config file owns, since that is the count an operator needs before reaching
// for the CLI.
func TestBucketsHeader_CountsReadOnly(t *testing.T) {
	t.Parallel()
	m := bucketsModel(t)

	got := m.bucketsHeaderView()
	for _, want := range []string{"2 declared", "1 from config", "read-only"} {
		if !strings.Contains(got, want) {
			t.Errorf("header missing %q: %q", want, got)
		}
	}
}

// TestBucketsHeader_NoConfigBuckets verifies a deployment declaring everything
// through the API does not carry a misleading zero.
func TestBucketsHeader_NoConfigBuckets(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.applyBuckets(&adminapi.ProvisioningResponse{
		Buckets: []adminapi.Bucket{{Name: "only", Source: adminapi.SourceStore}},
	})

	if got := m.bucketsHeaderView(); strings.Contains(got, "from config") {
		t.Errorf("header = %q, want no read-only note", got)
	}
}

// TestBucketsBody_Empty verifies a deployment declaring nothing says so rather
// than rendering an empty table.
func TestBucketsBody_Empty(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20
	m.applyBuckets(&adminapi.ProvisioningResponse{})

	if got := m.bucketsBodyView(); !strings.Contains(got, "no buckets declared") {
		t.Errorf("body = %q, want the empty notice", got)
	}
}

// TestBucketsBody_RendersNotices verifies what the merge of the two sources
// found reaches the pane, so a dangling grant is visible without the server log.
func TestBucketsBody_RendersNotices(t *testing.T) {
	t.Parallel()
	m := bucketsModel(t)
	m.buckets.notices = []adminapi.Notice{{Kind: "dangling_grant", Detail: "grant names bucket \"gone\""}}

	got := m.bucketsBodyView()
	if !strings.Contains(got, "dangling_grant") || !strings.Contains(got, "gone") {
		t.Errorf("body swallowed the notice:\n%s", got)
	}
}

// TestBucketsPaneView_Renders drives the whole pane so the header, table and
// footer compose without panicking on a sized terminal.
func TestBucketsPaneView_Renders(t *testing.T) {
	t.Parallel()
	m := bucketsModel(t)

	got := m.bucketsPaneView()
	for _, want := range []string{"buckets", "from-store", "reload"} {
		if !strings.Contains(got, want) {
			t.Errorf("pane missing %q:\n%s", want, got)
		}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// TestHandleBucketsKey_BackAndReload verifies esc returns focus to the nav and
// r issues a fetch.
func TestHandleBucketsKey_BackAndReload(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionBuckets

	m.handleBucketsKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !m.navFocus || m.navCursor != int(sectionBuckets) {
		t.Errorf("after esc: navFocus=%v cursor=%d", m.navFocus, m.navCursor)
	}

	m.navFocus = false
	_, cmd := m.handleBucketsKey(key("r"))
	if cmd == nil {
		t.Fatal("reload returned no command")
	}
	if _, ok := cmd().(bucketsLoadedMsg); !ok {
		t.Errorf("reload result = %#v, want bucketsLoadedMsg", cmd())
	}
}

// TestSelectSection_Buckets verifies entering the section loads it, so the pane
// is never shown against a stale snapshot from a previous visit.
func TestSelectSection_Buckets(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 20

	_, cmd := m.selectSection(sectionBuckets)
	if !m.buckets.loading {
		t.Error("entering the section did not set loading")
	}
	if cmd == nil {
		t.Fatal("entering the section issued no fetch")
	}
	if _, ok := cmd().(bucketsLoadedMsg); !ok {
		t.Errorf("fetch result = %#v, want bucketsLoadedMsg", cmd())
	}
}

// TestLoadBuckets_Error verifies a failed fetch reaches the model as an error
// message rather than a nil snapshot.
func TestLoadBuckets_Error(t *testing.T) {
	t.Parallel()
	cmd := initialModel(errLister{}).loadBuckets()
	if _, ok := cmd().(bucketsErrMsg); !ok {
		t.Errorf("cmd result = %#v, want bucketsErrMsg", cmd())
	}
}

// TestApplyBucketsErr_SeparatesUnavailable verifies a deployment whose
// provisioning half is not wired renders as a notice, and anything else as an
// error, so a 503 does not read as a broken instance.
func TestApplyBucketsErr_SeparatesUnavailable(t *testing.T) {
	t.Parallel()

	m := initialModel(&fakeLister{})
	m.applyBucketsErr(&adminclient.Error{Status: http.StatusServiceUnavailable, Body: `{"error":"not wired"}`})
	if m.buckets.unavailable == "" {
		t.Error("a 503 was recorded as an error rather than a notice")
	}
	if m.buckets.err != nil {
		t.Errorf("err = %v, want nil for an unavailable endpoint", m.buckets.err)
	}

	m = initialModel(&fakeLister{})
	m.applyBucketsErr(errors.New("connection refused"))
	if m.buckets.err == nil {
		t.Error("a transport failure was swallowed as a notice")
	}
}

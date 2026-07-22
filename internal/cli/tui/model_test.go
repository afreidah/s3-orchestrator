// -------------------------------------------------------------------------------
// TUI - Model Unit Tests
//
// Author: Alex Freidah
//
// Deterministic tests of the model's pure helpers and Update branches: prefix
// math, body rendering per state, page folding, navigation branches, and the
// error path. The end-to-end loop is covered separately in tui_test.go.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// errLister always fails, for the load error paths.
type errLister struct{}

func (errLister) ListObjects(_ context.Context, _, _ string) (*adminapi.ObjectListResponse, error) {
	return nil, errors.New("nope")
}

func (errLister) GetObjectLocations(_ context.Context, _ string) (*adminapi.ObjectLocationsResponse, error) {
	return nil, errors.New("nope")
}

func (errLister) GetStatus(_ context.Context) (*adminapi.StatusResponse, error) {
	return nil, errors.New("nope")
}

func (errLister) GetLogs(_ context.Context, _ string) (*adminapi.LogsResponse, error) {
	return nil, errors.New("nope")
}

func (errLister) GetReplicationStatus(_ context.Context) (*adminapi.ReplicationStatusResponse, error) {
	return nil, errors.New("nope")
}

func (errLister) ReconcileUsage(_ context.Context) error { return errors.New("nope") }
func (errLister) FlushCache(_ context.Context) error      { return errors.New("nope") }

// modelWith builds a model seeded with entries and a table synced to them.
func modelWith(entries []entry, prefix string, client adminClient) *model {
	m := initialModel(client)
	m.prefix = prefix
	m.entries = entries
	m.table.SetColumns([]table.Column{
		{Title: "NAME", Width: 20},
		{Title: "TYPE", Width: 5},
		{Title: "SIZE", Width: 12},
	})
	m.refreshVisible()
	return m
}

// TestApplyPage_BeforeResizeDoesNotPanic pins the fix for a race where the Init
// object load is delivered before the first WindowSizeMsg: the table must
// already have columns so SetRows does not panic in the row renderer.
func TestApplyPage_BeforeResizeDoesNotPanic(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{}) // width 0, no WindowSizeMsg yet
	m.applyPage(objectsLoadedMsg{prefix: "", page: &adminapi.ObjectListResponse{
		Objects: []adminapi.ObjectEntry{{Key: "a"}, {Key: "b"}},
	}})
	if len(m.entries) != 2 {
		t.Errorf("entries = %d, want 2", len(m.entries))
	}
}

func TestParentPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"", ""},
		{"photos/", ""},
		{"photos/2024/", "photos/"},
		{"a/b/c/", "a/b/"},
	}
	for _, c := range cases {
		if got := parentPrefix(c.in); got != c.want {
			t.Errorf("parentPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBodyView(t *testing.T) {
	t.Parallel()
	if got := (&model{err: errors.New("boom")}).bodyView(); !strings.Contains(got, "boom") {
		t.Errorf("error body = %q", got)
	}
	if got := (&model{loading: true, spinner: spinner.New()}).bodyView(); !strings.Contains(got, "loading") {
		t.Errorf("loading body = %q", got)
	}
	if got := (&model{}).bodyView(); !strings.Contains(got, "empty") {
		t.Errorf("empty body = %q", got)
	}
	// loaded rows all filtered out renders a distinct notice
	if got := (&model{entries: []entry{{name: "a"}}}).bodyView(); !strings.Contains(got, "no matches") {
		t.Errorf("no-matches body = %q", got)
	}
}

func TestApplyPage_ReplaceThenAppend(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "", &fakeLister{})

	m.applyPage(objectsLoadedMsg{prefix: "p/", page: &adminapi.ObjectListResponse{
		Objects: []adminapi.ObjectEntry{{Key: "p/a"}, {Key: "p/b"}}, Truncated: true, Next: "p/b",
	}})
	if m.prefix != "p/" || len(m.entries) != 2 || m.next != "p/b" {
		t.Fatalf("after replace: prefix=%q entries=%d next=%q", m.prefix, len(m.entries), m.next)
	}

	m.applyPage(objectsLoadedMsg{prefix: "p/", continuation: "p/b", page: &adminapi.ObjectListResponse{
		Objects: []adminapi.ObjectEntry{{Key: "p/c"}},
	}})
	if len(m.entries) != 3 || m.next != "" {
		t.Fatalf("after append: entries=%d next=%q", len(m.entries), m.next)
	}
}

func TestUpdate_ErrMsg(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.loading = true
	next, _ := m.Update(errMsg{err: errors.New("boom")})
	nm := next.(*model)
	if nm.err == nil || nm.loading {
		t.Errorf("err = %v, loading = %v; want error set and loading false", nm.err, nm.loading)
	}
}

func TestLoadObjects_Error(t *testing.T) {
	t.Parallel()
	cmd := initialModel(errLister{}).loadObjects("p/", "")
	if _, ok := cmd().(errMsg); !ok {
		t.Errorf("cmd result = %#v, want errMsg", cmd())
	}
}

func TestDescendAscendBranches(t *testing.T) {
	t.Parallel()
	f := &fakeLister{pages: map[string]*adminapi.ObjectListResponse{
		"photos/|": {Objects: []adminapi.ObjectEntry{{Key: "photos/x"}}},
		"|":        {CommonPrefixes: []string{"photos/"}},
	}}
	entries := []entry{{name: "photos/", isDir: true}, {name: "file"}}

	// descend on a directory (cursor 0) loads the child prefix
	m := modelWith(entries, "", f)
	if _, cmd := m.descend(); cmd == nil {
		t.Error("descend on dir: expected a load command")
	} else if msg, ok := cmd().(objectsLoadedMsg); !ok || msg.prefix != "photos/" {
		t.Errorf("descend result = %#v", cmd())
	}

	// descend on an object (cursor 1) is a no-op
	m = modelWith(entries, "", f)
	m.table.SetCursor(1)
	if _, cmd := m.descend(); cmd != nil {
		t.Error("descend on object: expected no command")
	}

	// ascend from a nested prefix loads the parent
	m = modelWith(entries, "photos/", f)
	if _, cmd := m.ascend(); cmd == nil {
		t.Error("ascend: expected a load command")
	} else if msg, ok := cmd().(objectsLoadedMsg); !ok || msg.prefix != "" {
		t.Errorf("ascend result = %#v", cmd())
	}

	// ascend at the root is a no-op
	m = modelWith(entries, "", f)
	if _, cmd := m.ascend(); cmd != nil {
		t.Error("ascend at root: expected no command")
	}
}

func TestHandleKey_QuitAndReload(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "p/", &fakeLister{})
	if _, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); cmd == nil {
		t.Fatal("quit: nil command")
	} else if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("quit result = %#v, want QuitMsg", cmd())
	}

	m = modelWith(nil, "p/", &fakeLister{})
	if _, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); cmd == nil || !m.loading {
		t.Errorf("reload: cmd=%v loading=%v", cmd, m.loading)
	}
}

func TestHandleKey_TableDelegationAndUnknownMsg(t *testing.T) {
	t.Parallel()
	// a movement key with no further pages falls through to the table
	m := modelWith([]entry{{name: "a"}, {name: "b"}}, "", &fakeLister{})
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.table.Cursor() != 1 {
		t.Errorf("cursor = %d, want 1 after down", m.table.Cursor())
	}
	// an unrecognised message type is a no-op
	if next, cmd := m.Update(struct{}{}); next == nil || cmd != nil {
		t.Errorf("unknown msg: next=%v cmd=%v", next, cmd)
	}
}

func TestResolveTarget(t *testing.T) {
	// flags win and the address gets an http prefix
	addr, tok, err := resolveTarget([]string{"-addr", "host:9000", "-token", "tok"})
	if err != nil || addr != "http://host:9000" || tok != "tok" {
		t.Fatalf("flags: addr=%q tok=%q err=%v", addr, tok, err)
	}

	// an explicit scheme is left untouched
	if addr, _, _ := resolveTarget([]string{"-addr", "https://x", "-token", "t"}); addr != "https://x" {
		t.Errorf("scheme passthrough: addr=%q", addr)
	}

	// an unknown flag surfaces the parse error
	if _, _, err := resolveTarget([]string{"-nope"}); err == nil {
		t.Error("bad flag: expected error")
	}

	// with no flags/env and an unreadable config, resolution fails
	t.Setenv("S3O_ADMIN_ADDR", "")
	t.Setenv("S3O_ADMIN_TOKEN", "")
	if _, _, err := resolveTarget([]string{"-config", "/no/such/file.yaml"}); err == nil {
		t.Error("missing target: expected error")
	}
}

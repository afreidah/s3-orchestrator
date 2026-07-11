// -------------------------------------------------------------------------------
// TUI - Inspector Unit Tests
//
// Author: Alex Freidah
//
// Deterministic tests of the inspector pane: the browse-to-inspect transition,
// ledger folding, inspect-mode key routing, body rendering per state, and the
// formatting helpers. The end-to-end open-inspector flow is covered in
// tui_test.go.
// -------------------------------------------------------------------------------

package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

func TestOpen_ObjectInspectsDirDescends(t *testing.T) {
	t.Parallel()
	f := &fakeLister{
		pages:     map[string]*adminapi.ObjectListResponse{"photos/|": {}},
		locations: map[string]*adminapi.ObjectLocationsResponse{"file": {Key: "file"}},
	}
	entries := []entry{{name: "photos/", isDir: true}, {name: "file"}}

	// open on an object (cursor 1) switches to the inspector and loads its key
	m := modelWith(entries, "", f)
	m.table.SetCursor(1)
	_, cmd := m.open()
	if m.mode != modeInspect || m.insp.key != "file" {
		t.Fatalf("open on object: mode=%v key=%q", m.mode, m.insp.key)
	}
	if msg, ok := cmd().(locationsLoadedMsg); !ok || msg.resp.Key != "file" {
		t.Errorf("open on object result = %#v", cmd())
	}

	// open on a directory (cursor 0) descends and stays in browse mode
	m = modelWith(entries, "", f)
	_, cmd = m.open()
	if m.mode != modeBrowse {
		t.Errorf("open on dir: mode=%v, want browse", m.mode)
	}
	if msg, ok := cmd().(objectsLoadedMsg); !ok || msg.prefix != "photos/" {
		t.Errorf("open on dir result = %#v", cmd())
	}
}

func TestApplyLocations(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.applyLocations(&adminapi.ObjectLocationsResponse{
		Key:       "k",
		Locations: []adminapi.ObjectLocation{{Backend: "b1"}, {Backend: "b2"}},
	})
	if len(m.insp.locations) != 2 || m.insp.loading || m.insp.err != nil {
		t.Fatalf("locations=%d loading=%v err=%v", len(m.insp.locations), m.insp.loading, m.insp.err)
	}
	if got := len(m.insp.table.Rows()); got != 2 {
		t.Errorf("table rows = %d, want 2", got)
	}
}

func TestLoadLocations_Error(t *testing.T) {
	t.Parallel()
	cmd := initialModel(errLister{}).loadLocations("k")
	if _, ok := cmd().(locationsErrMsg); !ok {
		t.Errorf("cmd result = %#v, want locationsErrMsg", cmd())
	}
}

func TestUpdate_LocationsMsgs(t *testing.T) {
	t.Parallel()
	// a loaded ledger folds into the inspector
	m := initialModel(&fakeLister{})
	next, _ := m.Update(locationsLoadedMsg{resp: &adminapi.ObjectLocationsResponse{
		Key: "k", Locations: []adminapi.ObjectLocation{{Backend: "b1"}},
	}})
	if nm := next.(*model); len(nm.insp.locations) != 1 {
		t.Errorf("loaded: locations=%d, want 1", len(nm.insp.locations))
	}

	// a fetch error surfaces on the inspector, clearing loading
	m = initialModel(&fakeLister{})
	m.insp.loading = true
	next, _ = m.Update(locationsErrMsg{err: errors.New("boom")})
	if nm := next.(*model); nm.insp.err == nil || nm.insp.loading {
		t.Errorf("err: err=%v loading=%v", next.(*model).insp.err, next.(*model).insp.loading)
	}
}

func TestHandleInspectKey(t *testing.T) {
	t.Parallel()
	// esc returns to the browser
	m := modelWith(nil, "p/", &fakeLister{})
	m.mode = modeInspect
	if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); m.mode != modeBrowse {
		t.Errorf("esc: mode=%v, want browse", m.mode)
	}

	// reload re-fetches and marks loading
	m = modelWith(nil, "p/", &fakeLister{})
	m.mode = modeInspect
	m.insp.key = "k"
	if _, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); cmd == nil || !m.insp.loading {
		t.Errorf("reload: cmd=%v loading=%v", cmd, m.insp.loading)
	}

	// quit still quits from the inspector
	m = modelWith(nil, "p/", &fakeLister{})
	m.mode = modeInspect
	if _, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); cmd == nil {
		t.Fatal("quit: nil command")
	} else if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("quit result = %#v, want QuitMsg", cmd())
	}

	// an unhandled key delegates to the copy table (cursor moves down)
	m = modelWith(nil, "p/", &fakeLister{})
	m.width, m.height = 80, 24
	m.mode = modeInspect
	m.insp.table = newTable()
	m.resizeInspector()
	m.applyLocations(&adminapi.ObjectLocationsResponse{Locations: []adminapi.ObjectLocation{{Backend: "b1"}, {Backend: "b2"}}})
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.insp.table.Cursor() != 1 {
		t.Errorf("cursor = %d, want 1 after down", m.insp.table.Cursor())
	}
}

func TestInspectBodyView(t *testing.T) {
	t.Parallel()
	if got := (&model{insp: inspector{err: errors.New("boom")}}).inspectBodyView(); !strings.Contains(got, "boom") {
		t.Errorf("error body = %q", got)
	}
	if got := (&model{spinner: spinner.New(), insp: inspector{loading: true}}).inspectBodyView(); !strings.Contains(got, "loading") {
		t.Errorf("loading body = %q", got)
	}
	if got := (&model{}).inspectBodyView(); !strings.Contains(got, "no copies") {
		t.Errorf("empty body = %q", got)
	}
}

func TestRelativeAge(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Time{}, "-"},
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-48 * time.Hour), "2d ago"},
	}
	for _, c := range cases {
		if got := relativeAge(c.in); got != c.want {
			t.Errorf("relativeAge(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTruncateAndYesNo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exact12chars", 12, "exact12chars"},
		{"truncateme", 5, "trun~"},
		{"x", 1, "x"},
		{"xy", 1, "x"}, // n <= 1 and too long: hard cut, no tilde
	}
	for _, c := range cases {
		if got := truncate(c.s, c.n); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
	if yesNo(true) != "yes" || yesNo(false) != "no" {
		t.Errorf("yesNo: %q / %q", yesNo(true), yesNo(false))
	}
}

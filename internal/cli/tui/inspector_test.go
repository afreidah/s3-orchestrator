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

// TestVerifiedAge_NeverIsItsOwnAnswer keeps an unverified copy from rendering
// like a missing field. "-" reads as no data; "never" is the actual finding.
func TestVerifiedAge_NeverIsItsOwnAnswer(t *testing.T) {
	t.Parallel()
	if got := verifiedAge(nil); got != "never" {
		t.Errorf("verifiedAge(nil) = %q, want %q", got, "never")
	}
	checked := time.Now().Add(-3 * time.Hour)
	if got := verifiedAge(&checked); got != "3h ago" {
		t.Errorf("verifiedAge(3h ago) = %q, want %q", got, "3h ago")
	}
}

// TestRowsFromLocations_VerifiedIsPerCopy is the asymmetry the pane exists to
// show: one copy checked last night and another never looked at is exactly the
// state that matters when a backend has been misbehaving.
func TestRowsFromLocations_VerifiedIsPerCopy(t *testing.T) {
	t.Parallel()
	checked := time.Now().Add(-2 * time.Hour)
	rows := rowsFromLocations([]adminapi.ObjectLocation{
		{Backend: "b1", LastScrubbedAt: &checked},
		{Backend: "b2"},
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per copy", len(rows))
	}
	last := len(rows[0]) - 1
	if rows[0][last] != "2h ago" || rows[1][last] != "never" {
		t.Errorf("verified column = %q/%q, want 2h ago/never", rows[0][last], rows[1][last])
	}
}

// TestArmScrubKey_ConfirmSaysWhatItCosts guards the wording. A prompt that reads
// as a read-only check would surprise an operator whose copy gets discarded.
func TestArmScrubKey_ConfirmSaysWhatItCosts(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "p/", &fakeLister{})
	m.mode = modeInspect
	m.insp.key = "bucket/k"

	if _, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")}); cmd != nil {
		t.Error("scrub ran without a confirmation")
	}
	if m.confirm == nil {
		t.Fatal("S did not arm a confirmation")
	}
	if !strings.Contains(m.confirm.text, "bucket/k") || !strings.Contains(m.confirm.text, "discarded") {
		t.Errorf("confirm = %q, want the key and what a failure costs", m.confirm.text)
	}
}

// TestArmScrubKey_AcceptMarksItRunning checks the pane reports the scrub the
// moment it is accepted. The request runs off the main loop, so without this
// the accepted keypress would look like it did nothing until the reply landed.
func TestArmScrubKey_AcceptMarksItRunning(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "p/", &fakeLister{
		scrubbed: &adminapi.ScrubKeyResponse{
			Key:    "bucket/k",
			Copies: []adminapi.CopyScrubResult{{Backend: "b1", Outcome: adminapi.CopyVerified}},
		},
	})
	m.mode = modeInspect
	m.insp.key = "bucket/k"
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")})

	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if !m.insp.scrubbing {
		t.Error("accepting the confirmation did not mark the scrub as running")
	}
	if cmd == nil {
		t.Fatal("accepting the confirmation dispatched nothing")
	}
	msg, ok := cmd().(scrubKeyMsg)
	if !ok || msg.err != nil || len(msg.resp.Copies) != 1 {
		t.Errorf("cmd result = %#v, want the key's verdicts", cmd())
	}
}

// TestArmScrubKey_NoKeyIsANoop covers the inspector opened on nothing.
func TestArmScrubKey_NoKeyIsANoop(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "p/", &fakeLister{})
	m.mode = modeInspect
	if _, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")}); cmd != nil || m.confirm != nil {
		t.Errorf("armed a scrub with no key: cmd=%v confirm=%v", cmd, m.confirm)
	}
}

// TestArmScrubKey_IgnoredWhileScrubbing keeps a held key from queueing a second
// pass over copies the first one is still reading.
func TestArmScrubKey_IgnoredWhileScrubbing(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "p/", &fakeLister{})
	m.mode = modeInspect
	m.insp.key = "bucket/k"
	m.insp.scrubbing = true

	if _, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")}); cmd != nil || m.confirm != nil {
		t.Errorf("armed a second scrub mid-flight: cmd=%v confirm=%v", cmd, m.confirm)
	}
}

// TestInspectHeaderView_ReportsScrubInFlight keeps a scrub that takes a while
// from looking like a keypress that did nothing.
func TestInspectHeaderView_ReportsScrubInFlight(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "p/", &fakeLister{})
	m.width, m.height = 100, 24
	m.insp.key = "bucket/k"

	if got := m.inspectHeaderView(); strings.Contains(got, "verifying") {
		t.Errorf("idle header claims a scrub is running: %q", got)
	}
	m.insp.scrubbing = true
	if got := m.inspectHeaderView(); !strings.Contains(got, "verifying") {
		t.Errorf("header = %q, want it to report the scrub in flight", got)
	}
}

// TestApplyScrubKey_NamesTheFailingBackend is the point of a targeted scrub:
// the summary has to say which copy is bad, since that is what the operator
// acts on. It also reloads, because a discarded copy is gone from the ledger.
func TestApplyScrubKey_NamesTheFailingBackend(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "p/", &fakeLister{})
	m.insp.key = "bucket/k"
	m.insp.scrubbing = true

	_, cmd := m.applyScrubKey(scrubKeyMsg{resp: &adminapi.ScrubKeyResponse{
		Key: "bucket/k",
		Copies: []adminapi.CopyScrubResult{
			{Backend: "b1", Outcome: adminapi.CopyVerified},
			{Backend: "b2", Outcome: adminapi.CopyMismatch},
		},
	}})

	if m.insp.scrubbing {
		t.Error("scrubbing flag survived the result")
	}
	if m.status == nil || m.status.ok {
		t.Fatalf("status = %+v, want a failure", m.status)
	}
	if !strings.Contains(m.status.text, "b2") || !strings.Contains(m.status.text, adminapi.CopyMismatch) {
		t.Errorf("status = %q, want the failing backend named", m.status.text)
	}
	if cmd == nil || !m.insp.loading {
		t.Errorf("ledger was not reloaded: cmd=%v loading=%v", cmd, m.insp.loading)
	}
}

// TestApplyScrubKey_AllVerified renders the clean case as a success.
func TestApplyScrubKey_AllVerified(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "p/", &fakeLister{})
	m.insp.key = "bucket/k"

	m.applyScrubKey(scrubKeyMsg{resp: &adminapi.ScrubKeyResponse{
		Key: "bucket/k",
		Copies: []adminapi.CopyScrubResult{
			{Backend: "b1", Outcome: adminapi.CopyVerified},
			{Backend: "b2", Outcome: adminapi.CopyVerified},
		},
	}})

	if m.status == nil || !m.status.ok {
		t.Fatalf("status = %+v, want a success", m.status)
	}
	if !strings.Contains(m.status.text, "2 copies verified") {
		t.Errorf("status = %q, want a verified count", m.status.text)
	}
}

// TestApplyScrubKey_Error reports the failure and leaves the ledger alone,
// since nothing about it changed.
func TestApplyScrubKey_Error(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "p/", &fakeLister{})
	m.insp.scrubbing = true

	_, cmd := m.applyScrubKey(scrubKeyMsg{err: errors.New("boom")})
	if m.status == nil || m.status.ok || !strings.Contains(m.status.text, "boom") {
		t.Fatalf("status = %+v, want the error surfaced", m.status)
	}
	if cmd != nil || m.insp.loading {
		t.Errorf("reloaded after a failed scrub: cmd=%v loading=%v", cmd, m.insp.loading)
	}
}

// TestUpdate_ScrubKeyMsg wires the message through the model's dispatch.
func TestUpdate_ScrubKeyMsg(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	next, _ := m.Update(scrubKeyMsg{resp: &adminapi.ScrubKeyResponse{
		Copies: []adminapi.CopyScrubResult{{Backend: "b1", Outcome: adminapi.CopyVerified}},
	}})
	if nm := next.(*model); nm.status == nil || !nm.status.ok {
		t.Errorf("status = %+v, want a success", next.(*model).status)
	}
}

// TestScrubKeyCmd_Error reports a transport failure as a message rather than
// leaving the pane stuck on "verifying".
func TestScrubKeyCmd_Error(t *testing.T) {
	t.Parallel()
	cmd := initialModel(errLister{}).scrubKey("k")
	msg, ok := cmd().(scrubKeyMsg)
	if !ok || msg.err == nil {
		t.Errorf("cmd result = %#v, want a failed scrubKeyMsg", cmd())
	}
}

// -------------------------------------------------------------------------------
// TUI - Browser Model Tests
//
// Author: Alex Freidah
//
// Drives the Bubble Tea model with teatest against a fake lister: asserts the
// load/render round-trip, prefix navigation, and load-more pagination without
// touching the network.
// -------------------------------------------------------------------------------

package tui

import (
	"bytes"
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// fakeLister returns canned pages keyed by "prefix|continuation".
type fakeLister struct {
	pages map[string]*adminapi.ObjectListResponse
}

func (f *fakeLister) ListObjects(_ context.Context, prefix, continuation string) (*adminapi.ObjectListResponse, error) {
	if p, ok := f.pages[prefix+"|"+continuation]; ok {
		return p, nil
	}
	return &adminapi.ObjectListResponse{}, nil
}

// waitForText fails the test unless the given text appears in the output.
func waitForText(t *testing.T, tm *teatest.TestModel, text string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte(text))
	}, teatest.WithDuration(3*time.Second))
}

// TestBrowser_LoadsAndDescends covers the initial load and descending into a
// directory: the child page's object appears and the prefix advances.
func TestBrowser_LoadsAndDescends(t *testing.T) {
	f := &fakeLister{pages: map[string]*adminapi.ObjectListResponse{
		"|":        {CommonPrefixes: []string{"photos/"}, Objects: []adminapi.ObjectEntry{{Key: "readme", Size: 10}}},
		"photos/|": {Objects: []adminapi.ObjectEntry{{Key: "photos/sunset", Size: 5}}},
	}}
	tm := teatest.NewTestModel(t, initialModel(f), teatest.WithInitialTermSize(80, 24))

	waitForText(t, tm, "photos")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // descend into the highlighted photos/
	waitForText(t, tm, "sunset")

	tm.Type("q")
	fm, ok := tm.FinalModel(t).(*model)
	if !ok {
		t.Fatal("final model is not a model")
	}
	if fm.prefix != "photos/" {
		t.Errorf("prefix = %q, want photos/", fm.prefix)
	}
}

// TestBrowser_LoadMoreAppends covers pagination: a truncated first page plus a
// scroll to the bottom pulls the continuation page and appends its rows.
func TestBrowser_LoadMoreAppends(t *testing.T) {
	f := &fakeLister{pages: map[string]*adminapi.ObjectListResponse{
		"|":     {Objects: []adminapi.ObjectEntry{{Key: "alpha"}, {Key: "beta"}}, Truncated: true, Next: "beta"},
		"|beta": {Objects: []adminapi.ObjectEntry{{Key: "gamma"}}},
	}}
	tm := teatest.NewTestModel(t, initialModel(f), teatest.WithInitialTermSize(80, 24))

	waitForText(t, tm, "beta")
	tm.Send(tea.KeyMsg{Type: tea.KeyDown}) // reach the bottom row, triggering load-more
	waitForText(t, tm, "gamma")

	tm.Type("q")
	fm, ok := tm.FinalModel(t).(*model)
	if !ok {
		t.Fatal("final model is not a model")
	}
	if len(fm.entries) != 3 {
		t.Errorf("entries = %d, want 3 (alpha, beta, gamma)", len(fm.entries))
	}
}

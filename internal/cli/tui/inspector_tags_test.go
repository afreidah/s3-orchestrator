// -------------------------------------------------------------------------------
// TUI Inspector - Tag Display Tests
//
// Author: Alex Freidah
//
// Tags are context beside the copy ledger rather than part of it, so what these
// cover is that they render under the table and that a failed tag read leaves
// the ledger - the thing the pane exists for - intact.
// -------------------------------------------------------------------------------

package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// TestInspectTagsView_RendersPairs verifies a loaded set renders as key=value
// pairs on one line.
func TestInspectTagsView_RendersPairs(t *testing.T) {
	t.Parallel()
	m := &model{}
	m.insp.tags = []adminapi.ObjectTag{
		{Key: "retain", Value: "30d"},
		{Key: "team", Value: "infra"},
	}

	got := m.inspectTagsView()
	for _, want := range []string{"retain=30d", "team=infra"} {
		if !strings.Contains(got, want) {
			t.Errorf("tags line %q missing %q", got, want)
		}
	}
}

// TestInspectTagsView_UntaggedObject verifies an object carrying no tags says
// so rather than rendering an empty line the operator cannot interpret.
func TestInspectTagsView_UntaggedObject(t *testing.T) {
	t.Parallel()
	m := &model{}

	if got := m.inspectTagsView(); !strings.Contains(got, "(none)") {
		t.Errorf("tags line = %q, want it to report no tags", got)
	}
}

// TestInspectView_TagsSurviveTheFrame renders the pane the way the program
// does rather than calling the tag helper on its own. The frame clamps the body
// to the window height, so a tag line rendered past that clamp is dropped, and
// on screen that is indistinguishable from never having rendered it.
func TestInspectView_TagsSurviveTheFrame(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "p/", &fakeLister{})
	m.width, m.height = 120, 24
	m.mode = modeInspect
	m.insp.table = newTable()
	m.resizeInspector()
	m.applyLocations(&adminapi.ObjectLocationsResponse{
		Locations: []adminapi.ObjectLocation{{Backend: "minio-1"}, {Backend: "minio-2"}},
	})
	m.applyTags(tagsLoadedMsg{tags: []adminapi.ObjectTag{{Key: "retain", Value: "30d"}}})

	if got := m.inspectView(); !strings.Contains(got, "retain=30d") {
		t.Errorf("inspectView dropped the tag line:\n%s", got)
	}
}

// TestApplyTags_LoadFailureKeepsTheLedger verifies a failed tag read leaves
// the inspector's error line alone. Blanking it would hide the copy ledger
// behind a message about something the operator did not open the pane for.
func TestApplyTags_LoadFailureKeepsTheLedger(t *testing.T) {
	t.Parallel()
	m := &model{}
	m.insp.locations = []adminapi.ObjectLocation{{Backend: "b1"}}

	m.applyTags(tagsLoadedMsg{err: errors.New("tag read failed")})

	if m.insp.err != nil {
		t.Errorf("inspector error set by a tag failure: %v", m.insp.err)
	}
	if len(m.insp.locations) != 1 {
		t.Errorf("copy ledger disturbed by a tag failure: %+v", m.insp.locations)
	}
	if len(m.insp.tags) != 0 {
		t.Errorf("expected no tags after a failed read, got %+v", m.insp.tags)
	}
}

// TestApplyTags_StoresLoadedSet verifies a successful read lands on the pane.
func TestApplyTags_StoresLoadedSet(t *testing.T) {
	t.Parallel()
	m := &model{}

	m.applyTags(tagsLoadedMsg{tags: []adminapi.ObjectTag{{Key: "a", Value: "1"}}})

	if len(m.insp.tags) != 1 || m.insp.tags[0].Key != "a" {
		t.Errorf("tags = %+v, want the loaded set", m.insp.tags)
	}
}

// TestLoadTags_ReportsTheError verifies the command surfaces a client failure
// as a message rather than panicking or returning a nil set silently.
func TestLoadTags_ReportsTheError(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{tagsErr: errors.New("boom")})

	msg, ok := m.loadTags("k")().(tagsLoadedMsg)
	if !ok {
		t.Fatal("expected a tagsLoadedMsg")
	}
	if msg.err == nil {
		t.Error("expected the client error to be carried")
	}
}

// TestLoadTags_CarriesTheSet verifies the command returns what the client gave
// it, keyed by the object under inspection.
func TestLoadTags_CarriesTheSet(t *testing.T) {
	t.Parallel()
	f := &fakeLister{tags: map[string][]adminapi.ObjectTag{
		"k": {{Key: "retain", Value: "30d"}},
	}}
	m := initialModel(f)

	msg, ok := m.loadTags("k")().(tagsLoadedMsg)
	if !ok {
		t.Fatal("expected a tagsLoadedMsg")
	}
	if msg.err != nil {
		t.Fatalf("unexpected error: %v", msg.err)
	}
	if len(msg.tags) != 1 || msg.tags[0].Key != "retain" {
		t.Errorf("tags = %+v, want the seeded set", msg.tags)
	}
}

// -------------------------------------------------------------------------------
// TUI Inspector - Tag Tests
//
// Author: Alex Freidah
//
// Tags are context beside the copy ledger rather than part of it, so these
// cover that they render under the table, that a failed tag read leaves the
// ledger - the thing the pane exists for - intact, and that editing and
// clearing a set reach the server and the pane.
// -------------------------------------------------------------------------------

package tui

import (
	"errors"
	"slices"
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
	m.insp.table = newTable(inspectorColumns)
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

// tagsModel opens the inspector on bucket/k carrying tags, for the edit and
// clear tests.
func tagsModel(t *testing.T, f *fakeLister, tags []adminapi.ObjectTag) *model {
	t.Helper()
	m := modelWith(nil, "", f)
	m.mode = modeInspect
	m.insp = inspector{key: "bucket/k", tags: tags, table: newTable(inspectorColumns)}
	return m
}

// TestEditTags_PrefillsAndReplaces verifies t opens the prompt on the current
// set in its query-string form, and that submitting replaces the whole set
// and shows what the server stored.
func TestEditTags_PrefillsAndReplaces(t *testing.T) {
	t.Parallel()
	f := &fakeLister{}
	m := tagsModel(t, f, []adminapi.ObjectTag{{Key: "a", Value: "1"}, {Key: "b", Value: "x y"}})

	m.handleInspectKey(key("t"))
	if got := m.prompt.input.Value(); got != "a=1&b=x+y" {
		t.Fatalf("prefill = %q, want the current set encoded", got)
	}

	_, cmd := submit(t, m, "team=data+eng&env=prod")
	status, _ := finish(t, m, cmd())
	if !status.ok || status.text != "tags saved on bucket/k" {
		t.Errorf("status = %+v, want the save reported", status)
	}
	want := []adminapi.ObjectTag{{Key: "env", Value: "prod"}, {Key: "team", Value: "data eng"}}
	if !slices.Equal(f.tagsSent, want) || !slices.Equal(m.insp.tags, want) {
		t.Errorf("sent %+v, shown %+v, want %+v", f.tagsSent, m.insp.tags, want)
	}
}

// TestEditTags_MalformedTextNeverReachesTheServer verifies a set that does not
// decode is reported as a failure without a write.
func TestEditTags_MalformedTextNeverReachesTheServer(t *testing.T) {
	t.Parallel()
	f := &fakeLister{}
	m := tagsModel(t, f, nil)

	m.handleInspectKey(key("t"))
	_, cmd := submit(t, m, "a=1&a=2")
	status, _ := finish(t, m, cmd())
	if status.ok || !strings.Contains(status.text, "duplicate tag key") {
		t.Errorf("status = %+v, want the duplicate refused", status)
	}
	if len(f.tagWrites) != 0 {
		t.Errorf("writes = %v, want none", f.tagWrites)
	}
}

// TestClearTags_ConfirmsAndClears verifies T asks first, clears the set, and
// does nothing on an object with no tags to clear.
func TestClearTags_ConfirmsAndClears(t *testing.T) {
	t.Parallel()
	f := &fakeLister{}
	m := tagsModel(t, f, []adminapi.ObjectTag{{Key: "a", Value: "1"}})

	m.handleInspectKey(key("T"))
	if m.confirm == nil || !strings.Contains(m.confirm.text, "bucket/k") {
		t.Fatalf("confirm = %+v, want it to name the key", m.confirm)
	}
	if status, _ := finish(t, m, accept(t, m)); status.text != "tags cleared on bucket/k" || len(m.insp.tags) != 0 {
		t.Errorf("status = %+v tags = %+v, want the set cleared", status, m.insp.tags)
	}

	m.handleInspectKey(key("T"))
	if m.confirm != nil {
		t.Error("clear armed on an object with no tags")
	}
	if !slices.Equal(f.tagWrites, []string{"clear bucket/k"}) {
		t.Errorf("writes = %v, want one clear", f.tagWrites)
	}
}

// TestTagsSaved_LeavesAnotherObjectAlone verifies a save that lands after the
// inspector moved to another key does not paint its set onto that key.
func TestTagsSaved_LeavesAnotherObjectAlone(t *testing.T) {
	t.Parallel()
	m := tagsModel(t, &fakeLister{}, nil)
	msg := m.saveTags("bucket/k", "a=1")()
	m.insp.key = "bucket/other"

	finish(t, m, msg)
	if len(m.insp.tags) != 0 {
		t.Errorf("tags = %+v, want the other object's set untouched", m.insp.tags)
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

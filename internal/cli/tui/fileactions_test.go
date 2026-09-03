// -------------------------------------------------------------------------------
// TUI - Files Pane Object Action Tests
//
// Author: Alex Freidah
//
// Covers the write actions on the object browser: the prompts each one arms,
// the confirmation a prefix delete builds from a counted page, and the two
// transfers, including that a failed download leaves nothing behind where a
// complete file would be.
// -------------------------------------------------------------------------------

package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// filesModel returns a model on the Files pane with one directory and one
// object loaded, the cursor on the object.
func filesModel(t *testing.T, f *fakeLister) *model {
	t.Helper()
	m := modelWith([]entry{
		{name: "photos/", isDir: true},
		{name: "readme.txt", size: 11},
	}, "bucket/", f)
	m.width, m.height = 120, 30
	return m
}

// submit types value into the armed prompt and submits it.
func submit(t *testing.T, m *model, value string) (tea.Model, tea.Cmd) {
	t.Helper()
	if m.prompt == nil {
		t.Fatal("no prompt was armed")
	}
	m.prompt.input.SetValue(value)
	return m.handleInputKey(tea.KeyMsg{Type: tea.KeyEnter})
}

// settle runs the transfer poll loop until the transfer reports done, so a
// test asserts on the finished state rather than on a race.
func settle(t *testing.T, m *model) transferDoneMsg {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, cmd := m.onTransferTick()
		if cmd == nil {
			t.Fatal("the transfer stopped being polled before it finished")
		}
		if done, ok := cmd().(transferDoneMsg); ok {
			return done
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the transfer did not finish within the deadline")
	return transferDoneMsg{}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestDownload_WritesTheObject asserts a download lands the body at the
// prompted path and reports what it wrote.
func TestDownload_WritesTheObject(t *testing.T) {
	t.Parallel()
	f := &fakeLister{payload: []byte("hello world")}
	m := filesModel(t, f)
	m.table.SetCursor(1) // the object, not the directory
	dest := filepath.Join(t.TempDir(), "readme.txt")

	m.handleBrowseKey(key("D"))
	if m.prompt == nil || !strings.Contains(m.prompt.text, "bucket/readme.txt") {
		t.Fatalf("prompt = %+v, want it to name the object", m.prompt)
	}
	submit(t, m, dest)

	if m.files.transfer == nil {
		t.Fatal("submitting the destination did not start a transfer")
	}
	done := settle(t, m)
	if done.err != nil {
		t.Fatalf("download: %v", done.err)
	}
	if f.downloaded != "bucket/readme.txt" {
		t.Errorf("fetched %q, want the full key", f.downloaded)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("wrote %q, want the object body", got)
	}

	m.applyTransferDone(done)
	if m.status == nil || !m.status.ok || !strings.Contains(m.status.text, "downloaded") {
		t.Errorf("status = %+v, want a download completion line", m.status)
	}
}

// TestDownload_FailureLeavesNoFile asserts an interrupted download cannot be
// mistaken for a complete one: the destination must not exist at all.
func TestDownload_FailureLeavesNoFile(t *testing.T) {
	t.Parallel()
	f := &fakeLister{downloadErr: errors.New("backend unavailable")}
	m := filesModel(t, f)
	m.table.SetCursor(1)
	dir := t.TempDir()
	dest := filepath.Join(dir, "readme.txt")

	m.handleBrowseKey(key("D"))
	submit(t, m, dest)
	done := settle(t, m)

	if done.err == nil {
		t.Fatal("the download reported success despite the transport failing")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("destination exists after a failed download (stat err = %v)", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed download left %d file(s) behind: %v", len(entries), entries)
	}

	m.applyTransferDone(done)
	if m.status == nil || m.status.ok || !strings.Contains(m.status.text, "backend unavailable") {
		t.Errorf("status = %+v, want the failure explained", m.status)
	}
}

// TestUpload_StoresTheLocalFile asserts the two prompts chain and the file
// reaches the key the operator typed, with the size the endpoint needs.
func TestUpload_StoresTheLocalFile(t *testing.T) {
	t.Parallel()
	f := &fakeLister{}
	m := filesModel(t, f)
	local := filepath.Join(t.TempDir(), "upload.bin")
	if err := os.WriteFile(local, []byte("payload!"), 0o600); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	m.handleBrowseKey(key("U"))
	_, cmd := submit(t, m, local)
	if cmd == nil {
		t.Fatal("the local-file prompt produced no follow-up")
	}
	// the first answer asks the loop for the second question
	next, ok := cmd().(uploadKeyPromptMsg)
	if !ok {
		t.Fatalf("msg = %#v, want uploadKeyPromptMsg", cmd())
	}
	m.askUploadKey(next.local)
	if m.prompt == nil || !strings.Contains(m.prompt.input.Value(), "bucket/") {
		t.Fatalf("key prompt = %+v, want it seeded with the current prefix", m.prompt)
	}
	submit(t, m, "bucket/uploaded.bin")

	done := settle(t, m)
	if done.err != nil {
		t.Fatalf("upload: %v", done.err)
	}
	if f.uploaded != "bucket/uploaded.bin" {
		t.Errorf("stored under %q, want the typed key", f.uploaded)
	}
	if string(f.uploadedBytes) != "payload!" {
		t.Errorf("stored %q, want the file contents", f.uploadedBytes)
	}
	if f.uploadedSize != int64(len("payload!")) {
		t.Errorf("declared size %d, want %d", f.uploadedSize, len("payload!"))
	}
}

// TestUpload_MissingLocalFileReports asserts a path that does not exist fails
// with a message rather than silently doing nothing.
func TestUpload_MissingLocalFileReports(t *testing.T) {
	t.Parallel()
	m := filesModel(t, &fakeLister{})

	m.handleBrowseKey(key("U"))
	_, cmd := submit(t, m, filepath.Join(t.TempDir(), "absent.bin"))
	next := cmd().(uploadKeyPromptMsg)
	m.askUploadKey(next.local)
	submit(t, m, "bucket/absent.bin")

	done := settle(t, m)
	if done.err == nil {
		t.Fatal("uploading a missing file reported success")
	}
	m.applyTransferDone(done)
	if m.status == nil || m.status.ok {
		t.Errorf("status = %+v, want a failure line", m.status)
	}
}

// TestDeleteObject_ConfirmsByKey asserts one object confirms against its full
// key and removes exactly that key.
func TestDeleteObject_ConfirmsByKey(t *testing.T) {
	t.Parallel()
	f := &fakeLister{}
	m := filesModel(t, f)
	m.table.SetCursor(1)

	m.handleBrowseKey(key("X"))
	if m.confirm == nil || !strings.Contains(m.confirm.text, "bucket/readme.txt") {
		t.Fatalf("confirm = %+v, want it to name the key", m.confirm)
	}
	_, cmd := m.handleConfirmKey(key("y"))
	msg := cmd().(objectDeletedMsg)
	if msg.err != nil || f.deletedKey != "bucket/readme.txt" {
		t.Errorf("deleted %q (err %v), want the highlighted key", f.deletedKey, msg.err)
	}

	_, reload := m.applyObjectDeleted(msg)
	if reload == nil {
		t.Error("a delete should reload the listing it removed a row from")
	}
}

// TestDeletePrefix_CountsBeforeConfirming asserts the operator is told how many
// objects a collapsed directory holds before agreeing to remove it.
func TestDeletePrefix_CountsBeforeConfirming(t *testing.T) {
	t.Parallel()
	f := &fakeLister{flatPages: map[string]*adminapi.ObjectListResponse{
		"bucket/photos/": {Objects: []adminapi.ObjectEntry{
			{Key: "bucket/photos/a"}, {Key: "bucket/photos/b"}, {Key: "bucket/photos/c"},
		}},
	}}
	m := filesModel(t, f)
	m.table.SetCursor(0) // the directory

	_, cmd := m.handleBrowseKey(key("X"))
	if m.confirm != nil {
		t.Fatal("a prefix delete must count before it confirms")
	}
	counted := cmd().(prefixCountedMsg)
	if counted.count != 3 {
		t.Fatalf("count = %d, want 3", counted.count)
	}

	m.applyPrefixCounted(counted)
	if m.confirm == nil || !strings.Contains(m.confirm.text, "3 objects") {
		t.Fatalf("confirm = %+v, want the count in the question", m.confirm)
	}

	_, run := m.handleConfirmKey(key("y"))
	deleted := run().(prefixDeletedMsg)
	if f.deletedPrefix != "bucket/photos/" {
		t.Errorf("deleted prefix %q, want bucket/photos/", f.deletedPrefix)
	}
	m.applyPrefixDeleted(deleted)
	if m.status == nil || !m.status.ok {
		t.Errorf("status = %+v, want the removal reported", m.status)
	}
}

// TestDeletePrefix_TruncatedCountReadsAsAFloor asserts a page that did not
// reach the end says "at least", so the operator is not shown a total that
// understates what will go.
func TestDeletePrefix_TruncatedCountReadsAsAFloor(t *testing.T) {
	t.Parallel()
	m := filesModel(t, &fakeLister{})

	m.applyPrefixCounted(prefixCountedMsg{prefix: "bucket/photos/", count: 1000, atLeast: true})
	if m.confirm == nil || !strings.Contains(m.confirm.text, "at least 1,000") {
		t.Errorf("confirm = %+v, want the count stated as a floor", m.confirm)
	}
}

// TestDeletePrefix_EmptyPrefixSaysSo asserts a directory with nothing under it
// reports that instead of arming a confirmation for zero objects.
func TestDeletePrefix_EmptyPrefixSaysSo(t *testing.T) {
	t.Parallel()
	m := filesModel(t, &fakeLister{})

	m.applyPrefixCounted(prefixCountedMsg{prefix: "bucket/photos/", count: 0})
	if m.confirm != nil {
		t.Error("an empty prefix should not arm a delete confirmation")
	}
	if m.status == nil || !strings.Contains(m.status.text, "nothing under") {
		t.Errorf("status = %+v, want an empty-prefix notice", m.status)
	}
}

// TestDeletePrefix_CountFailureReports asserts a failed count refuses to guess
// and reports why.
func TestDeletePrefix_CountFailureReports(t *testing.T) {
	t.Parallel()
	m := filesModel(t, &fakeLister{})

	m.applyPrefixCounted(prefixCountedMsg{prefix: "bucket/photos/", err: errors.New("ledger unavailable")})
	if m.confirm != nil {
		t.Error("a failed count must not arm a delete")
	}
	if m.status == nil || m.status.ok {
		t.Errorf("status = %+v, want the failure surfaced", m.status)
	}
}

// TestDownload_RefusesDirectory asserts the download key does nothing on a
// directory row, which has no bytes of its own.
func TestDownload_RefusesDirectory(t *testing.T) {
	t.Parallel()
	m := filesModel(t, &fakeLister{})
	m.table.SetCursor(0)

	m.handleBrowseKey(key("D"))
	if m.prompt != nil {
		t.Error("a directory should not arm a download prompt")
	}
}

// TestTransferLine_ReportsProgress asserts the running line carries the moved
// and total bytes, which is the whole point of polling.
func TestTransferLine_ReportsProgress(t *testing.T) {
	t.Parallel()
	m := filesModel(t, &fakeLister{})
	if got := m.fileTransferLine(); got != "" {
		t.Errorf("line = %q, want nothing while idle", got)
	}

	tr := &transfer{kind: transferDownload, key: "bucket/readme.txt", total: 2048}
	tr.moved.Store(1024)
	m.files.transfer = tr

	got := m.fileTransferLine()
	if !strings.Contains(got, "downloading") || !strings.Contains(got, "50%") {
		t.Errorf("line = %q, want the direction and the percentage", got)
	}
}

// TestPercentOf_CapsAtComplete asserts a body longer than its declared length
// cannot render past done.
func TestPercentOf_CapsAtComplete(t *testing.T) {
	t.Parallel()
	cases := []struct {
		moved, total int64
		want         int
	}{
		{0, 0, 0},
		{512, 1024, 50},
		{2048, 1024, 100},
	}
	for _, tc := range cases {
		if got := percentOf(tc.moved, tc.total); got != tc.want {
			t.Errorf("percentOf(%d, %d) = %d, want %d", tc.moved, tc.total, got, tc.want)
		}
	}
}

// TestFileActions_DispatchThroughUpdate asserts each message this pane raises
// is routed by the model, not just handled when called directly.
func TestFileActions_DispatchThroughUpdate(t *testing.T) {
	t.Parallel()
	m := filesModel(t, &fakeLister{})

	cases := []struct {
		name string
		msg  tea.Msg
		want func() bool
	}{
		{
			name: "prefix counted",
			msg:  prefixCountedMsg{prefix: "bucket/photos/", count: 2},
			want: func() bool { return m.confirm != nil },
		},
		{
			name: "object deleted",
			msg:  objectDeletedMsg{key: "bucket/readme.txt"},
			want: func() bool { return m.status != nil && m.status.ok },
		},
		{
			name: "prefix deleted",
			msg:  prefixDeletedMsg{prefix: "bucket/photos/", deleted: 2},
			want: func() bool { return m.status != nil && strings.Contains(m.status.text, "2 objects") },
		},
		{
			name: "transfer done",
			msg:  transferDoneMsg{kind: transferDownload, key: "bucket/readme.txt", local: "/tmp/readme.txt", moved: 11},
			want: func() bool { return m.status != nil && strings.Contains(m.status.text, "downloaded") },
		},
		{
			name: "upload key prompt",
			msg:  uploadKeyPromptMsg{local: "/tmp/up.bin"},
			want: func() bool { return m.prompt != nil && strings.Contains(m.prompt.text, "up.bin") },
		},
	}

	for _, tc := range cases {
		m.confirm, m.status, m.prompt = nil, nil, nil
		if _, _ = m.Update(tc.msg); !tc.want() {
			t.Errorf("%s: the model did not route the message", tc.name)
		}
	}
}

// TestFileActions_ReportFailures asserts each failed action explains itself
// rather than reporting a silent no-op.
func TestFileActions_ReportFailures(t *testing.T) {
	t.Parallel()
	m := filesModel(t, &fakeLister{})

	if _, cmd := m.applyObjectDeleted(objectDeletedMsg{key: "bucket/readme.txt", err: errors.New("backend refused")}); cmd != nil {
		t.Error("a failed delete should not reload the listing")
	}
	if m.status == nil || m.status.ok || !strings.Contains(m.status.text, "backend refused") {
		t.Errorf("status = %+v, want the failure surfaced", m.status)
	}

	if _, cmd := m.applyPrefixDeleted(prefixDeletedMsg{prefix: "bucket/photos/", err: errors.New("half removed")}); cmd != nil {
		t.Error("a failed prefix delete should not reload the listing")
	}
	if m.status == nil || m.status.ok {
		t.Errorf("status = %+v, want the failure surfaced", m.status)
	}
}

// TestDeleteObject_EmptyListingIsNoop asserts the delete key does nothing when
// there is no row under the cursor.
func TestDeleteObject_EmptyListingIsNoop(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "bucket/", &fakeLister{})

	if _, cmd, handled := m.handleFileActionKey("X"); !handled || cmd != nil {
		t.Errorf("delete on an empty listing: handled=%v cmd=%v, want handled with nothing to do", handled, cmd)
	}
	if m.confirm != nil {
		t.Error("an empty listing should not arm a confirmation")
	}
}

// TestTransferKind_Words pins the wording each direction reports with, since
// the completion line is the only place an operator sees which way bytes went.
func TestTransferKind_Words(t *testing.T) {
	t.Parallel()
	if transferDownload.verb() != "downloading" || transferDownload.past() != "downloaded" {
		t.Error("download wording wrong")
	}
	if transferUpload.verb() != "uploading" || transferUpload.past() != "uploaded" {
		t.Error("upload wording wrong")
	}
}

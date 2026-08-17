// -------------------------------------------------------------------------------
// TUI - Files Pane Object Actions
//
// Author: Alex Freidah
//
// Write actions on the highlighted row of the object browser: download it to a
// prompted local path, upload a local file under a prompted key, remove one
// object, or remove everything under a directory. Every destructive action
// confirms first and names what it will affect, and a prefix delete counts the
// keys first so the operator is not agreeing to a number they cannot see.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/afreidah/s3-orchestrator/internal/util/humanize"
)

// The confirmation counts one page of keys under the prefix. A page is enough
// to convey the scale, and walking the whole subtree before the operator has
// agreed to anything would be a lot of work to answer a question they may
// answer with "no". A truncated page reports the count as a floor.

// fileAction is a write action armed against the highlighted row.
type fileAction struct {
	transfer *transfer // set while bytes are moving, nil otherwise
}

// prefixCountedMsg carries the key count behind a prefix, and the confirmation
// it should arm.
type prefixCountedMsg struct {
	prefix  string
	count   int
	atLeast bool // the page was truncated, so the real total is higher
	err     error
}

// uploadKeyPromptMsg asks the loop for the second half of an upload: the key
// the chosen local file should be stored under.
type uploadKeyPromptMsg struct{ local string }

// objectDeletedMsg reports one object removed.
type objectDeletedMsg struct {
	key string
	err error
}

// prefixDeletedMsg reports a prefix removed and how many objects went with it.
type prefixDeletedMsg struct {
	prefix  string
	deleted int
	err     error
}

// countPrefix returns a command that counts the keys under prefix, so the
// confirmation can state the scale of what is about to be removed.
func (m *model) countPrefix(prefix string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		count, atLeast, err := countKeysUnder(client, prefix)
		return prefixCountedMsg{prefix: prefix, count: count, atLeast: atLeast, err: err}
	}
}

// countKeysUnder reads one flat page under prefix. Reports whether the page
// was truncated, so the caller states the count as a floor rather than
// understating what the delete will remove.
func countKeysUnder(client adminClient, prefix string) (count int, atLeast bool, err error) {
	page, err := client.ListObjectsFlat(context.Background(), prefix, "")
	if err != nil {
		return 0, false, err
	}
	return len(page.Objects), page.Truncated, nil
}

// deleteObject returns a command that removes one object.
func (m *model) deleteObject(key string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		_, err := client.DeleteObject(context.Background(), key)
		return objectDeletedMsg{key: key, err: err}
	}
}

// deletePrefix returns a command that removes everything under prefix.
func (m *model) deletePrefix(prefix string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.DeletePrefix(context.Background(), prefix)
		if err != nil {
			return prefixDeletedMsg{prefix: prefix, err: err}
		}
		return prefixDeletedMsg{prefix: prefix, deleted: resp.Deleted}
	}
}

// -------------------------------------------------------------------------
// KEYS
// -------------------------------------------------------------------------

// handleFileActionKey arms the action bound to key against the highlighted
// row, reporting whether the key was one of its own.
func (m *model) handleFileActionKey(key string) (tea.Model, tea.Cmd, bool) {
	switch key {
	case "D":
		return m.armDownload()
	case "U":
		return m.armUpload()
	case "X":
		return m.armDelete()
	}
	return m, nil, false
}

// selectedEntry reports the highlighted row and its full key.
func (m *model) selectedEntry() (entry, string, bool) {
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.visible) {
		return entry{}, "", false
	}
	row := m.visible[idx]
	return row, m.prefix + row.name, true
}

// armDownload asks where to write the highlighted object, defaulting the
// prompt to its base name in the working directory.
func (m *model) armDownload() (tea.Model, tea.Cmd, bool) {
	row, key, ok := m.selectedEntry()
	if !ok || row.isDir {
		return m, nil, true // a directory has no bytes of its own to fetch
	}

	model, cmd := m.askForValue("Download "+key+" to?", filepath.Base(key), func(dest string) adminAction {
		return adminAction{
			before: func(m *model) { m.beginTransfer(startDownload(m.client, key, dest)) },
			run:    transferTick(),
		}
	})
	return model, cmd, true
}

// armUpload asks for the local file, then for the key to store it under. The
// second question is armed from the update loop rather than nested inside the
// first, since a prompt is model state and only the loop may set it.
func (m *model) armUpload() (tea.Model, tea.Cmd, bool) {
	model, cmd := m.askFor("Upload which local file?", "/path/to/file", func(local string) adminAction {
		return adminAction{run: func() tea.Msg { return uploadKeyPromptMsg{local: local} }}
	})
	return model, cmd, true
}

// askUploadKey arms the second half of an upload: where the chosen file should
// land. The key is seeded with the prefix on screen, since that is the
// directory the operator is looking at.
func (m *model) askUploadKey(local string) (tea.Model, tea.Cmd) {
	return m.askForValue("Store "+filepath.Base(local)+" under which key?", m.prefix+filepath.Base(local),
		func(key string) adminAction {
			return adminAction{
				before: func(m *model) { m.beginTransfer(startUpload(m.client, key, local)) },
				run:    transferTick(),
			}
		})
}

// armDelete confirms removing the highlighted row: one object directly, or a
// directory once its keys have been counted.
func (m *model) armDelete() (tea.Model, tea.Cmd, bool) {
	row, key, ok := m.selectedEntry()
	if !ok {
		return m, nil, true
	}
	if row.isDir {
		m.status = &actionStatus{ok: true, text: "counting objects under " + key + "..."}
		count := m.countPrefix(key)
		return m, count, true
	}

	model, cmd := m.startAction(adminAction{
		confirm: "Delete " + key + "?",
		run:     m.deleteObject(key),
	})
	return model, cmd, true
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// updateFileActions handles the messages these actions raise, reporting
// whether the message was one of them.
func (m *model) updateFileActions(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case prefixCountedMsg:
		model, cmd := m.applyPrefixCounted(msg)
		return model, cmd, true
	case uploadKeyPromptMsg:
		model, cmd := m.askUploadKey(msg.local)
		return model, cmd, true
	case objectDeletedMsg:
		model, cmd := m.applyObjectDeleted(msg)
		return model, cmd, true
	case prefixDeletedMsg:
		model, cmd := m.applyPrefixDeleted(msg)
		return model, cmd, true
	case transferTickMsg:
		model, cmd := m.onTransferTick()
		return model, cmd, true
	case transferDoneMsg:
		model, cmd := m.applyTransferDone(msg)
		return model, cmd, true
	}
	return m, nil, false
}

// applyPrefixCounted arms the prefix-delete confirmation now that the scale is
// known, so the operator agrees to a number rather than to a directory name.
func (m *model) applyPrefixCounted(msg prefixCountedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = &actionStatus{ok: false, text: "count " + msg.prefix + ": " + msg.err.Error()}
		return m, nil
	}
	if msg.count == 0 {
		m.status = &actionStatus{ok: true, text: "nothing under " + msg.prefix}
		return m, nil
	}

	scale := grouped(msg.count)
	if msg.atLeast {
		scale = "at least " + scale
	}
	m.status = nil
	return m.startAction(adminAction{
		confirm: fmt.Sprintf("Delete %s objects under %s?", scale, msg.prefix),
		run:     m.deletePrefix(msg.prefix),
	})
}

// applyObjectDeleted reports one removal and reloads the listing it came from.
func (m *model) applyObjectDeleted(msg objectDeletedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = &actionStatus{ok: false, text: "delete " + msg.key + ": " + msg.err.Error()}
		return m, nil
	}
	m.status = &actionStatus{ok: true, text: "deleted " + msg.key}
	reload := m.reloadListing()
	return m, reload
}

// applyPrefixDeleted reports how many objects a prefix delete removed.
func (m *model) applyPrefixDeleted(msg prefixDeletedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = &actionStatus{ok: false, text: "delete " + msg.prefix + ": " + msg.err.Error()}
		return m, nil
	}
	m.status = &actionStatus{ok: true,
		text: fmt.Sprintf("deleted %s under %s", countOf(msg.deleted, "object", "objects"), msg.prefix)}
	reload := m.reloadListing()
	return m, reload
}

// reloadListing refetches the current prefix, so a listing cannot keep showing
// what was just removed.
func (m *model) reloadListing() tea.Cmd {
	m.loading = true
	return m.loadObjects(m.prefix, "")
}

// -------------------------------------------------------------------------
// TRANSFERS
// -------------------------------------------------------------------------

// beginTransfer shows the transfer starting and begins polling its progress.
func (m *model) beginTransfer(t *transfer) {
	m.files.transfer = t
}

// onTransferTick reads the running transfer's counter, reporting completion
// once the goroutine has finished. The ticker lapses when nothing is moving.
func (m *model) onTransferTick() (tea.Model, tea.Cmd) {
	t := m.files.transfer
	if t == nil {
		return m, nil
	}
	if t.done.Load() {
		return m, func() tea.Msg {
			return transferDoneMsg{kind: t.kind, key: t.key, local: t.local, moved: t.moved.Load(), err: t.err}
		}
	}
	return m, transferTick()
}

// applyTransferDone reports the finished transfer and clears the pane's hold
// on it. A failure names what went wrong; the partial file is already gone.
func (m *model) applyTransferDone(msg transferDoneMsg) (tea.Model, tea.Cmd) {
	m.files.transfer = nil
	if msg.err != nil {
		m.status = &actionStatus{ok: false,
			text: fmt.Sprintf("%s %s: %s", msg.kind.verb(), msg.key, msg.err.Error())}
		return m, nil
	}

	where := msg.local
	if msg.kind == transferUpload {
		where = msg.key
	}
	m.status = &actionStatus{ok: true,
		text: fmt.Sprintf("%s %s (%s)", msg.kind.past(), where, humanize.Bytes(msg.moved))}
	if msg.kind == transferUpload {
		reload := m.reloadListing()
		return m, reload
	}
	return m, nil
}

// fileTransferLine reports the running transfer's progress, or nothing when
// the pane is idle. The caller styles it, as it does the line it replaces.
func (m *model) fileTransferLine() string {
	if m.files.transfer == nil {
		return ""
	}
	return transferLine(m.files.transfer)
}

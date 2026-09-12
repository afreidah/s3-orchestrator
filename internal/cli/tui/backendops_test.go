// -------------------------------------------------------------------------------
// TUI - Per-Backend Action Menu Tests
//
// Author: Alex Freidah
//
// Covers the menu a highlighted backend opens: it is the ops pane scoped to one
// backend, so every request it sends names that backend and the ops section
// keeps meaning the whole fleet.
//
// The scoping is asserted on the request rather than the label, because the
// label is what an operator reads and the query is what decides which copies
// the pass rewrites.
// -------------------------------------------------------------------------------

package tui

import (
	"net/url"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// -------------------------------------------------------------------------
// SCOPING
// -------------------------------------------------------------------------

// TestScopeToBackend_NamesTheBackend asserts a scoped request carries the
// backend and a fleet-wide one carries nothing.
func TestScopeToBackend_NamesTheBackend(t *testing.T) {
	t.Parallel()

	scoped := scopeToBackend(opsRequest{path: pathCompressExisting}, "minio-a")
	if got := scoped.query.Get(queryBackend); got != "minio-a" {
		t.Errorf("backend = %q, want minio-a", got)
	}

	fleet := scopeToBackend(opsRequest{path: pathCompressExisting}, "")
	if fleet.query != nil {
		t.Errorf("query = %v, want none for a fleet-wide request", fleet.query)
	}
}

// TestScopeToBackend_KeepsTheRequestsOwnQuery asserts scoping an action that
// already resolved a query adds to it rather than replacing it, which is what
// the bounded-batch entries need.
func TestScopeToBackend_KeepsTheRequestsOwnQuery(t *testing.T) {
	t.Parallel()

	req := opsRequest{path: pathCompressExisting, query: url.Values{"max": {"1000"}}}
	got := scopeToBackend(req, "minio-a")

	if got.query.Get("max") != "1000" {
		t.Errorf("max = %q, want it kept", got.query.Get("max"))
	}
	if got.query.Get(queryBackend) != "minio-a" {
		t.Errorf("backend = %q, want minio-a", got.query.Get(queryBackend))
	}
}

// -------------------------------------------------------------------------
// THE MENU
// -------------------------------------------------------------------------

// TestBackendActions_OpenScopedToTheHighlightedRow asserts entering the menu
// from a backend row scopes it to that backend and remembers where to return.
func TestBackendActions_OpenScopedToTheHighlightedRow(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})

	m.handleBackendsKey(tea.KeyMsg{Type: tea.KeyEnter})

	if m.section != sectionOps {
		t.Fatalf("section = %v, want the ops pane", m.section)
	}
	if m.ops.backend != "minio-a" {
		t.Errorf("backend = %q, want the highlighted row", m.ops.backend)
	}
	if len(m.ops.actions) != len(backendActions()) {
		t.Errorf("actions = %d, want the per-backend list", len(m.ops.actions))
	}
}

// TestBackendActions_EveryEntryNamesTheBackend walks the whole menu and asserts
// each entry's request is scoped. An entry added later that forgets is what
// this catches.
func TestBackendActions_EveryEntryNamesTheBackend(t *testing.T) {
	t.Parallel()

	for i := range backendActions() {
		a := backendActions()[i]
		req := scopeToBackend(opsRequest{path: a.path}, "minio-a")
		if got := req.query.Get(queryBackend); got != "minio-a" {
			t.Errorf("%s: backend = %q, want minio-a", a.label, got)
		}
	}
}

// TestBackendActions_StreamTheirProgress asserts no entry decodes a single
// summary. Each reads and rewrites a backend's copies, so a caller watching one
// needs to see it move.
func TestBackendActions_StreamTheirProgress(t *testing.T) {
	t.Parallel()

	for _, a := range backendActions() {
		if a.result != nil {
			t.Errorf("%s decodes a one-shot summary, want a stream", a.label)
		}
	}
}

// TestOpsBack_ReturnsToTheBackendsPane asserts esc from a scoped menu goes back
// to the row it was opened from rather than to the nav.
func TestOpsBack_ReturnsToTheBackendsPane(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	m.handleBackendsKey(tea.KeyMsg{Type: tea.KeyEnter})

	m.handleOpsKey(key("esc"))

	if m.section != sectionBackends {
		t.Errorf("section = %v, want the backends pane", m.section)
	}
}

// TestOpsBack_FromTheOpsSectionReturnsToTheNav asserts the fleet-wide menu is
// unchanged: esc there steps back to the nav as it always did.
func TestOpsBack_FromTheOpsSectionReturnsToTheNav(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionOps
	m.ops.actions = opsActions()

	m.handleOpsKey(key("esc"))

	if !m.navFocus {
		t.Error("esc from the ops section did not return focus to the nav")
	}
}

// TestOpsHeader_NamesTheScopedBackend asserts the header says which backend the
// menu acts on, so a scoped pass cannot be read as the fleet-wide one.
func TestOpsHeader_NamesTheScopedBackend(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	m.handleBackendsKey(tea.KeyMsg{Type: tea.KeyEnter})

	if got := m.opsHeaderView(); !strings.Contains(got, "minio-a") {
		t.Errorf("header = %q, want the backend named", got)
	}
}

// TestOpsMenu_EncryptionEntriesStream pins the fleet-wide encryption entries as
// streaming, matching the compression pair. They rewrite every object in the
// fleet, so a spinner held for that long is indistinguishable from a hang.
func TestOpsMenu_EncryptionEntriesStream(t *testing.T) {
	t.Parallel()

	for _, a := range encryptionActions() {
		if a.path == pathEncryptExisting || a.path == pathDecryptExisting {
			if a.result != nil {
				t.Errorf("%s decodes a one-shot summary, want a stream", a.label)
			}
		}
	}
}

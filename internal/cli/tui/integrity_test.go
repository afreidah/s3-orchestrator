// -------------------------------------------------------------------------------
// TUI - Integrity View Tests
//
// Author: Alex Freidah
//
// Deterministic tests for the integrity pane: the row mapping, both listings
// loading together, the tab toggle, the management filter reaching the request
// and a load under a stale filter being dropped, header totals, the body per
// state and per tab, and the footer offering the filter only where it applies.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// listedCopy is one listed copy of key on backend.
func listedCopy(key, backend string, managed bool) adminapi.ListedCopy {
	return adminapi.ListedCopy{Key: key, ObjectLocation: adminapi.ObjectLocation{
		Backend: backend, SizeBytes: 2048, Managed: managed, CreatedAt: time.Now().Add(-time.Hour),
	}}
}

// integrityModel builds a sized model on the integrity section with both
// listings loaded, serving its listings from f.
func integrityModel(t *testing.T, f *fakeLister) *model {
	t.Helper()
	m := initialModel(f)
	m.width, m.height = 120, 20
	m.section = sectionIntegrity
	m.applyIntegrity(integrityListings{
		unhashed:   &adminapi.CopyListResponse{Total: 12, Copies: []adminapi.ListedCopy{listedCopy("a/1", "b1", true)}},
		unreadable: &adminapi.CopyListResponse{Total: 3, Copies: []adminapi.ListedCopy{listedCopy("lost", "b2", false)}},
	})
	return m
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

func TestCopyRow(t *testing.T) {
	t.Parallel()
	c := listedCopy("a/1", "b1", false)
	c.Encrypted = true
	if r := copyRow(&c); r[0] != "a/1" || r[1] != "b1" || r[2] != "2.0 KiB" || r[3] != "false" || r[4] != "true" || r[5] != "1h ago" {
		t.Errorf("row = %v", r)
	}
}

func TestApplyIntegrity(t *testing.T) {
	t.Parallel()
	m := integrityModel(t, &fakeLister{})
	if m.integrity.loading || m.integrity.err != nil || !m.integrity.fetched {
		t.Errorf("state = %+v", m.integrity)
	}
	if m.integrity.unhashedTotal != 12 || m.integrity.unreadableTotal != 3 {
		t.Errorf("totals = %d / %d, want 12 / 3", m.integrity.unhashedTotal, m.integrity.unreadableTotal)
	}
	if len(m.integrity.unhashed.table.Rows()) != 1 || len(m.integrity.unreadable.table.Rows()) != 1 {
		t.Errorf("table rows = %d / %d", len(m.integrity.unhashed.table.Rows()), len(m.integrity.unreadable.table.Rows()))
	}
}

// TestLoadIntegrity_FetchesBothListings verifies one load asks for the unhashed
// listing under the pane's filter and the unreadable one, and folds both in.
func TestLoadIntegrity_FetchesBothListings(t *testing.T) {
	t.Parallel()
	f := &fakeLister{
		unhashed:   &adminapi.CopyListResponse{Total: 5, Copies: []adminapi.ListedCopy{listedCopy("a/1", "b1", true)}},
		unreadable: &adminapi.CopyListResponse{Total: 1},
	}
	m := initialModel(f)
	m.Update(m.loadIntegrity()())
	if m.integrity.unhashedTotal != 5 || m.integrity.unreadableTotal != 1 || len(f.unhashedAsked) != 1 || f.unhashedAsked[0] != nil {
		t.Errorf("totals = %d / %d, asked %v; want 5 / 1 under no filter", m.integrity.unhashedTotal, m.integrity.unreadableTotal, f.unhashedAsked)
	}
}

func TestHandleIntegrityKey_TogglesTab(t *testing.T) {
	t.Parallel()
	m := integrityModel(t, &fakeLister{})
	if m.integrityOnUnreadable() {
		t.Fatal("pane should open on the unhashed listing")
	}
	m.handleIntegrityKey(key("t"))
	if !m.integrityOnUnreadable() {
		t.Error("t did not switch to the unreadable listing")
	}
	m.handleIntegrityKey(key("t"))
	if m.integrityOnUnreadable() {
		t.Error("t did not switch back to the unhashed listing")
	}
}

// TestHandleIntegrityKey_CyclesManagedAndRefetches verifies "m" steps the
// filter through managed, unmanaged and both, each step asking the server for
// the copies under the new filter.
func TestHandleIntegrityKey_CyclesManagedAndRefetches(t *testing.T) {
	t.Parallel()
	f := &fakeLister{}
	m := integrityModel(t, f)
	for _, want := range []string{"managed", "unmanaged", "all"} {
		_, cmd := m.handleIntegrityKey(key("m"))
		if cmd == nil {
			t.Fatalf("%s: no refetch", want)
		}
		m.Update(cmd())
		if got := managedLabel(m.integrity.managed); got != want {
			t.Errorf("filter = %s, want %s", got, want)
		}
		if asked := f.unhashedAsked[len(f.unhashedAsked)-1]; !sameManaged(asked, m.integrity.managed) {
			t.Errorf("%s: asked for %s", want, managedLabel(asked))
		}
	}
}

// TestHandleIntegrityKey_ManagedRefusedOnUnreadable covers pressing "m" on the
// listing it does not apply to: it explains itself rather than refetching.
func TestHandleIntegrityKey_ManagedRefusedOnUnreadable(t *testing.T) {
	t.Parallel()
	m := integrityModel(t, &fakeLister{})
	m.integrity.tab = integrityTabUnreadable
	if _, cmd := m.handleIntegrityKey(key("m")); cmd != nil || m.integrity.managed != nil {
		t.Errorf("m on the unreadable tab changed the filter to %s", managedLabel(m.integrity.managed))
	}
	if m.status == nil || !strings.Contains(m.status.text, "unhashed") {
		t.Errorf("status = %+v, want an explanation", m.status)
	}
}

// TestApplyIntegrity_DropsStaleFilter verifies a load asked for under a filter
// the operator has since changed does not overwrite the pane.
func TestApplyIntegrity_DropsStaleFilter(t *testing.T) {
	t.Parallel()
	m := integrityModel(t, &fakeLister{})
	m.integrity.managed = new(false)
	m.applyIntegrity(integrityListings{
		managed:    new(true),
		unhashed:   &adminapi.CopyListResponse{Total: 99},
		unreadable: &adminapi.CopyListResponse{},
	})
	if m.integrity.unhashedTotal != 12 {
		t.Errorf("unhashed total = %d, want the stale load dropped", m.integrity.unhashedTotal)
	}
}

// TestIntegrityHeaderView_ShowsBothTotals asserts the total of the listing that
// is not on screen stays visible, and the unhashed total names its filter.
func TestIntegrityHeaderView_ShowsBothTotals(t *testing.T) {
	t.Parallel()
	m := integrityModel(t, &fakeLister{})
	if out := m.integrityHeaderView(); !strings.Contains(out, "[unhashed 12 (all)]") || !strings.Contains(out, "unreadable 3") {
		t.Errorf("header = %q", out)
	}
	m.integrity.tab = integrityTabUnreadable
	if out := m.integrityHeaderView(); !strings.Contains(out, "[unreadable 3]") {
		t.Errorf("unreadable tab not marked active: %q", out)
	}
}

func TestIntegrityBody_States(t *testing.T) {
	t.Parallel()
	m := integrityModel(t, &fakeLister{})
	if got := bodyText(m.integrityBody()); !strings.Contains(got, "a/1") {
		t.Errorf("unhashed body = %q", got)
	}
	m.integrity.tab = integrityTabUnreadable
	if got := bodyText(m.integrityBody()); !strings.Contains(got, "lost") {
		t.Errorf("unreadable body = %q", got)
	}

	m.integrity = newIntegrityView()
	if got := bodyText(m.integrityBody()); !strings.Contains(got, "no unhashed copies") {
		t.Errorf("empty unhashed body = %q", got)
	}
	m.integrity.tab = integrityTabUnreadable
	if got := bodyText(m.integrityBody()); !strings.Contains(got, "no unreadable copies") {
		t.Errorf("empty unreadable body = %q", got)
	}
}

// TestIntegrityFooter_OffersFilterOnlyOnUnhashed asserts the hints do not
// advertise a filter the active listing does not take.
func TestIntegrityFooter_OffersFilterOnlyOnUnhashed(t *testing.T) {
	t.Parallel()
	m := integrityModel(t, &fakeLister{})
	if got := m.hintFooter(); !strings.Contains(got, "managed") {
		t.Errorf("unhashed footer = %q, want the managed filter", got)
	}
	m.integrity.tab = integrityTabUnreadable
	if got := m.hintFooter(); strings.Contains(got, "managed") {
		t.Errorf("unreadable footer offers the managed filter: %q", got)
	}
}

func TestLoadIntegrity_Error(t *testing.T) {
	t.Parallel()
	m := initialModel(errLister{})
	m.Update(m.loadIntegrity()())
	if m.integrity.err == nil {
		t.Error("a failed fetch should surface on the pane")
	}
}

// TestLoadIntegrity_UnreadableError verifies a failure of the second listing
// fails the load rather than showing the first alone.
func TestLoadIntegrity_UnreadableError(t *testing.T) {
	t.Parallel()
	m := initialModel(&unreadableFails{})
	m.Update(m.loadIntegrity()())
	if m.integrity.err == nil || m.integrity.fetched {
		t.Errorf("state = %+v, want the failure surfaced", m.integrity)
	}
}

// unreadableFails serves the unhashed listing and fails the unreadable one.
type unreadableFails struct{ fakeLister }

func (*unreadableFails) ListUnreadable(context.Context) (*adminapi.CopyListResponse, error) {
	return nil, errors.New("unreadable listing failed")
}

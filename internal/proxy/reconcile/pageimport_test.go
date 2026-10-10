// -------------------------------------------------------------------------------
// Reconcile - Listing Page Import Tests
//
// Author: Alex Freidah
//
// Covers what one page import does with each listed path: a recorded path or
// one with a delete outstanding is skipped without reading its bytes, a path
// the ledger does not hold is classified and imported, and a dry run reports
// only the paths it would import. Also covers the lookup and import failures.
// -------------------------------------------------------------------------------

package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// pageStore is a ListedPathImporter whose lookup reports states and whose
// imports report outcome, recording each key it was asked to import.
type pageStore struct {
	states    map[string]core.PathState
	lookupErr error
	outcome   core.ImportOutcome
	importErr error
	imported  []string
}

// ListedPathStates reports the fixture's states.
func (s *pageStore) ListedPathStates(context.Context, string, []string) (map[string]core.PathState, error) {
	return s.states, s.lookupErr
}

// ImportObject records the key and reports the fixture's outcome.
func (s *pageStore) ImportObject(_ context.Context, req *core.ImportObjectRequest) (core.ImportOutcome, error) {
	s.imported = append(s.imported, req.Key)
	return s.outcome, s.importErr
}

// GetAllObjectLocations reports no siblings, so a path is classified on its
// own bytes.
func (*pageStore) GetAllObjectLocations(context.Context, string) ([]core.ObjectLocation, error) {
	return nil, core.ErrObjectNotFound
}

// pageImporter builds an importer over store and an in-memory backend holding
// plaintext bytes for every listed key. Reads counts every ranged GET.
func pageImporter(store *pageStore, page []backend.ListedObject, dryRun bool) (PageImporter, *backendtest.InMemory) {
	be := backendtest.NewInMemory()
	for i := range page {
		be.Objects[page[i].Key] = backendtest.Object{Data: []byte("plaintext body")}
	}
	return PageImporter{
		Store: store,
		Classify: ClassifyDeps{
			Backend: be,
			Stores:  store,
			Source:  "test",
			Log:     slog.Default(),
		},
		BackendName: "b1",
		Prefixes:    []string{"vb/"},
		DryRun:      dryRun,
	}, be
}

// listed builds a listing page of the given keys, each size bytes.
func listed(size int64, keys ...string) []backend.ListedObject {
	page := make([]backend.ListedObject, len(keys))
	for i, k := range keys {
		page[i] = backend.ListedObject{Key: k, SizeBytes: size}
	}
	return page
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestPageImport_ImportsOnlyWhatTheLedgerLacks verifies recorded paths and
// paths with a delete outstanding are skipped without being read or
// imported, and only the miss is classified and imported.
func TestPageImport_ImportsOnlyWhatTheLedgerLacks(t *testing.T) {
	t.Parallel()
	store := &pageStore{
		states: map[string]core.PathState{
			"vb/recorded": core.PathRecorded,
			"vb/deleted":  core.PathPendingCleanup,
		},
		outcome: core.ImportInserted,
	}
	page := listed(10, "vb/recorded", "vb/deleted", "vb/new")
	imp, _ := pageImporter(store, page, false)

	res, err := imp.Import(context.Background(), page)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res != (PageResult{Imported: 1, Skipped: 2, ImportedBytes: 10}) {
		t.Errorf("result = %+v, want 1 imported of 10 bytes and 2 skipped", res)
	}
	if !slices.Equal(store.imported, []string{"vb/new"}) {
		t.Errorf("imported %v, want only vb/new", store.imported)
	}
}

// TestPageImport_DryRunReportsOnlyMisses verifies a dry run counts the paths
// it would import, skips the ones the ledger holds, and writes nothing.
func TestPageImport_DryRunReportsOnlyMisses(t *testing.T) {
	t.Parallel()
	store := &pageStore{states: map[string]core.PathState{"vb/a": core.PathRecorded}}
	page := listed(10, "vb/a", "vb/b", "vb/c")
	imp, _ := pageImporter(store, page, true)

	res, err := imp.Import(context.Background(), page)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res != (PageResult{Imported: 2, Skipped: 1, ImportedBytes: 20}) {
		t.Errorf("result = %+v, want 2 would-import of 20 bytes and 1 skipped", res)
	}
	if len(store.imported) != 0 {
		t.Errorf("dry run imported %v", store.imported)
	}
}

// TestPageImport_CountsAPendingDeleteTheImportCaught verifies a path the
// lookup missed but the import transaction refused, because a delete was
// queued in between, counts as skipped and adds no bytes.
func TestPageImport_CountsAPendingDeleteTheImportCaught(t *testing.T) {
	t.Parallel()
	store := &pageStore{outcome: core.ImportSkippedPendingCleanup}
	page := listed(7, "vb/x")
	imp, _ := pageImporter(store, page, false)

	res, err := imp.Import(context.Background(), page)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res != (PageResult{Skipped: 1}) {
		t.Errorf("result = %+v, want 1 skipped and no bytes", res)
	}
}

// TestPageImport_ReturnsLookupAndImportErrors verifies a failed lookup ends
// the page before anything is imported, and a failed import ends it with the
// counts gathered so far.
func TestPageImport_ReturnsLookupAndImportErrors(t *testing.T) {
	t.Parallel()
	want := errors.New("db down")

	lookupFails := &pageStore{lookupErr: want}
	imp, _ := pageImporter(lookupFails, listed(1, "vb/a"), false)
	if _, err := imp.Import(context.Background(), listed(1, "vb/a")); !errors.Is(err, want) {
		t.Errorf("lookup failure: err = %v, want %v", err, want)
	}
	if len(lookupFails.imported) != 0 {
		t.Errorf("imported %v after a failed lookup", lookupFails.imported)
	}

	importFails := &pageStore{outcome: core.ImportInserted, importErr: want}
	page := listed(1, "vb/a", "vb/b")
	imp, _ = pageImporter(importFails, page, false)
	if _, err := imp.Import(context.Background(), page); !errors.Is(err, want) {
		t.Errorf("import failure: err = %v, want %v", err, want)
	}
	if !slices.Equal(importFails.imported, []string{"vb/a"}) {
		t.Errorf("imported %v, want the page to stop at the first failure", importFails.imported)
	}
}

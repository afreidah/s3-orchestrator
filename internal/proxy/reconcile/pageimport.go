// -------------------------------------------------------------------------------
// Reconcile - Listing Page Import
//
// Author: Alex Freidah
//
// Imports one page of a backend listing. The ledger is asked about the whole
// page in one query, so a pass over a backend the ledger already tracks costs
// one lookup per page rather than a ranged GET and an import transaction per
// object. Only the paths the ledger does not hold are classified and imported,
// and the import transaction checks the path again, so a copy recorded between
// the lookup and the import is still left alone.
// -------------------------------------------------------------------------------

package reconcile

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// ListedPathImporter is the ledger surface a page import needs: what the
// ledger holds for the listed paths, and the import itself.
type ListedPathImporter interface {
	ListedPathStates(ctx context.Context, backend string, paths []string) (map[string]core.PathState, error)
	ImportObject(ctx context.Context, req *core.ImportObjectRequest) (core.ImportOutcome, error)
}

// PageImporter imports listing pages for one backend. Classify reaches the
// bytes of the paths that need importing. Prefixes are the configured bucket
// prefixes; a path outside all of them is imported unmanaged. DryRun reports
// what would be imported without classifying or writing anything.
type PageImporter struct {
	Store       ListedPathImporter
	Classify    ClassifyDeps
	BackendName string
	Prefixes    []string
	DryRun      bool
}

// PageResult counts one page: paths imported, paths skipped because the
// ledger already holds them or a delete is outstanding, and the bytes
// imported.
type PageResult struct {
	Imported      int
	Skipped       int
	ImportedBytes int64
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Import imports one listing page and reports what it did. An import failure
// ends the page with the counts gathered so far.
func (p *PageImporter) Import(ctx context.Context, objects []backend.ListedObject) (PageResult, error) {
	paths := make([]string, len(objects))
	for i := range objects {
		paths[i] = objects[i].Key
	}
	states, err := p.Store.ListedPathStates(ctx, p.BackendName, paths)
	if err != nil {
		return PageResult{}, fmt.Errorf("look up listed paths: %w", err)
	}

	var res PageResult
	for i := range objects {
		obj := &objects[i]
		switch states[obj.Key] {
		case core.PathPendingCleanup:
			p.warnPendingCleanup(ctx, obj.Key)
			res.Skipped++
			continue
		case core.PathRecorded:
			res.Skipped++
			continue
		}
		outcome, err := p.importMiss(ctx, obj)
		if err != nil {
			return res, fmt.Errorf("failed to import %s: %w", obj.Key, err)
		}
		switch outcome {
		case core.ImportInserted:
			res.Imported++
			res.ImportedBytes += obj.SizeBytes
		case core.ImportSkippedPendingCleanup:
			p.warnPendingCleanup(ctx, obj.Key)
			res.Skipped++
		default:
			res.Skipped++
		}
	}
	return res, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// importMiss classifies and imports one path the ledger did not hold, or
// reports it under a dry run as though it had been imported.
func (p *PageImporter) importMiss(ctx context.Context, obj *backend.ListedObject) (core.ImportOutcome, error) {
	unmanaged := Unmanaged(obj.Key, p.Prefixes)
	if p.DryRun {
		p.Classify.Log.InfoContext(ctx, "would import",
			"key", obj.Key, "size", obj.SizeBytes, "unmanaged", unmanaged)
		return core.ImportInserted, nil
	}
	return importListed(ctx, p.Classify, p.Store, &core.ImportObjectRequest{
		Key:       obj.Key,
		Backend:   p.BackendName,
		Size:      obj.SizeBytes,
		Unmanaged: unmanaged,
		WrittenAt: obj.LastModified,
	})
}

// warnPendingCleanup reports a listed path whose delete never reached the
// backend. Logged rather than folded silently into the skipped count: an
// operator seeing a run full of them is looking at a cleanup queue that is
// not draining.
func (p *PageImporter) warnPendingCleanup(ctx context.Context, key string) {
	p.Classify.Log.WarnContext(ctx, "skipping key with an outstanding delete",
		"key", key, "backend", p.BackendName)
}

// importListed classifies the bytes at one listed path and imports it. The
// sorted-merge reconcile and the page import both come through here, so a
// path is classified the same way whichever pass found it.
func importListed(ctx context.Context, deps ClassifyDeps, store ListedPathImporter, req *core.ImportObjectRequest) (core.ImportOutcome, error) {
	form, err := ClassifyImport(ctx, deps, req.Backend, req.Key, req.Size)
	if err != nil {
		return core.ImportSkippedExisting, err
	}
	req.Form = form
	return store.ImportObject(ctx, req)
}

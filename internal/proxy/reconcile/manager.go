// -------------------------------------------------------------------------------
// Reconcile - Backend Sync and Reconciliation Orchestration
//
// Author: Alex Freidah
//
// Drives the two ways a backend's real contents are folded back into the
// ledger: sync, which imports everything the backend holds, and reconcile,
// which diffs both sides and applies the difference in each direction.
//
// The merge engine and its streams live alongside this file; what this adds is
// the wiring - resolving the backend's lister, accounting the list calls
// against its API quota, and turning a stale ledger row into a delete plus a
// cleanup-queue sweep.
// -------------------------------------------------------------------------------

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

//go:generate mockgen -destination=mock_manager_test.go -package=reconcile github.com/afreidah/s3-orchestrator/internal/proxy/reconcile Stores,BackendResolver,UsageRecorder

// -------------------------------------------------------------------------
// CONSUMER INTERFACES
// -------------------------------------------------------------------------

// Stores is the store surface reconciliation needs: look up and import
// discovered keys, drop a stale row, walk the ledger in byte order, and sweep
// cleanup-queue rows belonging to a key that no longer exists.
type Stores interface {
	ImportObject(ctx context.Context, req *core.ImportObjectRequest) (core.ImportOutcome, error)
	ListedPathStates(ctx context.Context, backend string, paths []string) (map[string]core.PathState, error)
	GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error)
	DeleteObjectLocation(ctx context.Context, key, backendName string) (int64, error)
	ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterKey string, limit int) ([]core.ObjectLocation, error)
	SweepStaleCleanupQueueRows(ctx context.Context, storageKey, backendName string) (int64, error)
}

// BackendResolver looks up a configured backend by name.
// *infra.BackendRuntime satisfies it.
type BackendResolver interface {
	GetBackend(name string) (backend.ObjectBackend, error)
}

// UsageRecorder admits and accounts backend API calls against the usage
// quota, so a listing pass is held to the same limits as client traffic. Allow
// is asked per page, so a walk of a large bucket stops at the limit instead of
// spending the budget client requests need.
type UsageRecorder interface {
	Allow(backendName string, ops []s3op.Operation, egress, ingress int64) bool
	APICalls(op s3op.Operation, backendName string, n int64)
}

// listOp is what a reconcile spends: it walks a bucket one listing page at a
// time. Package-level so a walk of thousands of pages does not allocate it per
// page.
var listOp = []s3op.Operation{s3op.ListObjects}

// errBudgetExhausted stops a listing walk that has spent the backend's API
// budget. Returned from the page callback, which is the only way to end a walk
// early, and unwrapped by the caller: a pass that stopped at the limit did the
// work it could afford and is not a failure.
var errBudgetExhausted = errors.New("backend API budget exhausted mid-walk")

// -------------------------------------------------------------------------
// PAGE BUDGET
// -------------------------------------------------------------------------

// pageBudget charges a backend's API quota one listing page at a time and
// reports whether the walk may continue. Charging per page rather than once at
// the end is what lets a walk stop at the limit.
type pageBudget struct {
	usage       UsageRecorder
	backendName string
}

// charge records the page just consumed and reports whether the backend can
// afford another. The page is charged either way: the request already happened,
// and refusing to count it is how the ledger drifts from the provider's.
func (b pageBudget) charge() bool {
	// A nil usage leaves the walk unmetered.
	if b.usage == nil {
		return true
	}
	b.usage.APICalls(s3op.ListObjects, b.backendName, 1)
	return b.usage.Allow(b.backendName, listOp, 0, 0)
}

// reportBudgetStop records a walk that ended at the backend's usage limit. The
// counts are what the pass managed before stopping, not what the backend holds.
func (m *Manager) reportBudgetStop(ctx context.Context, op, backendName string) {
	telemetry.UsageLimitRejectionsTotal.WithLabelValues(op, "list").Inc()
	m.logger().WarnContext(ctx, op+" stopped at the backend's API usage limit",
		"backend", backendName,
		"detail", "the pass covered only part of the bucket; it resumes on the next run once the budget allows")
}

// -------------------------------------------------------------------------
// MANAGER
// -------------------------------------------------------------------------

// Manager orchestrates sync and reconcile passes for one fleet.
type Manager struct {
	backends BackendResolver
	stores   Stores
	usage    UsageRecorder
	quota    *counter.QuotaTracker
	codec    StoredInspector
	log      *slog.Logger
}

// Deps groups the reconcile manager's constructor parameters. Codec and Log are
// optional: without a codec a rediscovered compressed object is imported as the
// verbatim bytes it appears to be, and a nil logger resolves to the default at
// call time.
type Deps struct {
	Backends BackendResolver
	Stores   Stores
	Usage    UsageRecorder
	Quota    *counter.QuotaTracker
	Codec    StoredInspector
	Log      *slog.Logger
}

// NewManager builds a Manager.
func NewManager(d *Deps) *Manager {
	must.NotNil("d", d)
	must.NotNil("d.Backends", d.Backends)
	must.NotNil("d.Stores", d.Stores)
	must.NotNil("d.Usage", d.Usage)
	must.NotNil("d.Quota", d.Quota)
	return &Manager{
		backends: d.Backends,
		stores:   d.Stores,
		usage:    d.Usage,
		quota:    d.Quota,
		codec:    d.Codec,
		log:      d.Log,
	}
}

// logger returns the configured logger or the default.
func (m *Manager) logger() *slog.Logger {
	if m.log == nil {
		return slog.Default()
	}
	return m.log
}

// -------------------------------------------------------------------------
// SYNC AND RECONCILE
// -------------------------------------------------------------------------

// SyncBackend scans a backend's bucket and imports pre-existing objects into
// the ledger. Objects already tracked for the backend are skipped.
// knownBuckets is the full list of configured virtual bucket names; an object
// outside every one of their prefixes is imported at its own key and flagged
// unmanaged, so it counts toward the backend's quota without any worker acting
// on it. Returns counts of imported vs skipped objects.
func (m *Manager) SyncBackend(ctx context.Context, backendName, bucket string, knownBuckets []string) (imported, skipped int, err error) {
	s3b, err := m.backends.GetBackend(backendName)
	if err != nil {
		return 0, 0, err
	}

	// One page of headroom is the entry price, so a pass does not start against
	// a backend that is already spent. The per-page charge inside the walk is
	// what holds it to the limit from there.
	if !m.usage.Allow(backendName, listOp, 0, 0) {
		return 0, 0, fmt.Errorf("backend %s: %w", backendName, core.ErrUsageLimitExceeded)
	}

	m.logger().InfoContext(ctx, "starting backend sync", "backend", backendName, "bucket", bucket)

	pages := PageImporter{
		Store:       m.stores,
		Classify:    m.classifyDeps(s3b),
		BackendName: backendName,
		Prefixes:    BucketPrefixes(knownBuckets),
	}
	budget := pageBudget{usage: m.usage, backendName: backendName}

	err = s3b.ListObjects(ctx, "", func(objects []backend.ListedObject) error {
		// Charged before the import so the page is paid for even if importing
		// it fails: the listing request reached the provider either way.
		canContinue := budget.charge()
		page, pErr := pages.Import(ctx, objects)
		imported += page.Imported
		skipped += page.Skipped
		if pErr != nil {
			return pErr
		}
		if !canContinue {
			return errBudgetExhausted
		}
		return nil
	})

	if errors.Is(err, errBudgetExhausted) {
		m.reportBudgetStop(ctx, "sync", backendName)
		return imported, skipped, nil
	}
	if err != nil {
		return imported, skipped, err
	}

	m.logger().InfoContext(ctx, "backend sync complete", "backend", backendName, "bucket", bucket,
		"imported", imported, "skipped", skipped)
	return imported, skipped, nil
}

// importDiscovered records one key the sorted merge found only on a backend.
// Satisfies ImporterFn, and goes through the same classify-and-import step as
// the page import, so both passes classify identically.
func (m *Manager) importDiscovered(ctx context.Context, req *core.ImportObjectRequest) (core.ImportOutcome, error) {
	be, err := m.backends.GetBackend(req.Backend)
	if err != nil {
		return core.ImportSkippedExisting, err
	}
	return importListed(ctx, m.classifyDeps(be), m.stores, req)
}

// classifyDeps is how this manager's passes reach a backend's bytes and the
// ledger when classifying a discovered key.
func (m *Manager) classifyDeps(be backend.ObjectBackend) ClassifyDeps {
	return ClassifyDeps{
		Backend: be,
		Stores:  m.stores,
		Codec:   m.codec,
		Source:  "reconcile",
		Log:     m.logger(),
	}
}

// ReconcileBackend diffs a backend against the ledger with the bounded-memory
// sorted merge. It imports keys found only on the backend, marking those
// outside every configured bucket prefix unmanaged, and deletes ledger rows
// whose bytes are gone. One walk covers every virtual bucket in the backend's
// real bucket, and keys are compared exactly as stored so both streams stay in
// byte order.
func (m *Manager) ReconcileBackend(ctx context.Context, backendName string, knownBuckets []string) (*Result, error) {
	s3b, err := m.backends.GetBackend(backendName)
	if err != nil {
		return nil, err
	}

	// The same entry price SyncBackend pays, which this path did not ask for at
	// all: a reconcile could start against a backend with nothing left.
	if !m.usage.Allow(backendName, listOp, 0, 0) {
		return nil, fmt.Errorf("backend %s: %w", backendName, core.ErrUsageLimitExceeded)
	}

	s3 := NewS3KeyStream(ctx, s3b, BucketPrefixes(knownBuckets), m.usage, backendName)
	defer s3.Stop()

	dbIter := NewDBCursorStream(DBCursorStreamDeps{Store: m.stores, BackendName: backendName})
	defer dbIter.Stop()

	res := &Result{}
	mergeErr := Sorted(
		ctx, s3, dbIter,
		ImportHandler(m.logger(), backendName, m.importDiscovered, res),
		DeleteHandler(m.logger(), backendName, m.deleter(), res),
	)

	if errors.Is(mergeErr, errBudgetExhausted) {
		m.reportBudgetStop(ctx, "reconcile", backendName)
		return res, nil
	}
	if mergeErr != nil {
		return res, fmt.Errorf("reconcile %s: %w", backendName, mergeErr)
	}
	return res, nil
}

// deleter removes a stale ledger row and sweeps any cleanup-queue rows that
// referenced it. The sweep is best-effort: the row is already gone, so a
// failed sweep leaves queue rows that the next pass will retry rather than a
// reason to fail the reconcile.
func (m *Manager) deleter() DeleterFn {
	return func(ctx context.Context, key, storageKey, backendName string) error {
		// The delete credits the backend's stripes in its own transaction, so
		// there is nothing to tell the routing snapshot; it reloads on its tick.
		if _, err := m.stores.DeleteObjectLocation(ctx, key, backendName); err != nil {
			return err
		}
		// Swept by path: what this pass established is that these particular
		// bytes are absent, so only the deletions queued against them are moot.
		// Any queued against the key's other writes name bytes still on the
		// backend.
		if _, err := m.stores.SweepStaleCleanupQueueRows(ctx, storageKey, backendName); err != nil {
			m.logger().WarnContext(ctx, "failed to sweep cleanup_queue rows for stale copy",
				slog.String("key", key), slog.String("storage_key", storageKey),
				slog.String("backend", backendName), "error", err)
		}
		return nil
	}
}

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
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

//go:generate mockgen -destination=mock_manager_test.go -package=reconcile github.com/afreidah/s3-orchestrator/internal/proxy/reconcile Stores,BackendResolver,UsageRecorder

// Stores is the store surface reconciliation needs: import a discovered key,
// drop a stale row, walk the ledger in byte order, and sweep cleanup-queue
// rows belonging to a key that no longer exists.
type Stores interface {
	ImportObject(ctx context.Context, req *core.ImportObjectRequest) (core.ImportOutcome, error)
	GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error)
	DeleteObjectLocation(ctx context.Context, key, backendName string) error
	ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterKey string, limit int) ([]core.ObjectLocation, error)
	SweepStaleCleanupQueueRows(ctx context.Context, key, backendName string) (int64, error)
}

// BackendResolver looks up a configured backend by name.
// *infra.BackendRuntime satisfies it.
type BackendResolver interface {
	GetBackend(name string) (backend.ObjectBackend, error)
}

// UsageRecorder admits and accounts backend API calls against the usage
// quota, so a listing pass shows up in the same counters a client request
// would and is held to the same limits.
//
// Allow is asked per page rather than once before the walk: a reconcile of a
// large bucket is thousands of list requests, and a pass that runs the
// provider past its monthly request quota leaves client traffic to be refused
// on the budget it consumed.
type UsageRecorder interface {
	Allow(backendName string, apiCalls, egress, ingress int64) bool
	APICalls(backendName string, n int64)
}

// errBudgetExhausted stops a listing walk that has spent the backend's API
// budget. Returned from the page callback, which is the only way to end a walk
// early, and unwrapped by the caller: a pass that stopped at the limit did the
// work it could afford and is not a failure.
var errBudgetExhausted = errors.New("backend API budget exhausted mid-walk")

// pageBudget charges a backend's API quota one listing page at a time and
// reports whether the walk may continue.
//
// Charged as each page is consumed rather than accumulated for a single charge
// at the end. A walk that only records what it spent once it has finished
// cannot be stopped at the limit, and a bucket large enough to matter is
// thousands of requests - so the overage arrives as one step, after the fact,
// against a budget the rest of the fleet still has to share.
type pageBudget struct {
	usage       UsageRecorder
	backendName string
}

// charge records the page just consumed and reports whether the backend can
// afford another. The page is charged either way: the request already happened,
// and refusing to count it is how the ledger drifts from the provider's.
func (b pageBudget) charge() bool {
	// The zero value is unmetered, which is what a caller with no usage tracker
	// to charge against passes - the collation harness and the stream's own
	// tests, neither of which is talking to a real provider.
	if b.usage == nil {
		return true
	}
	b.usage.APICalls(b.backendName, 1)
	return b.usage.Allow(b.backendName, 1, 0, 0)
}

// reportBudgetStop records a walk that ended at the backend's usage limit. The
// counts are what the pass managed before stopping, not what the backend holds.
func (m *Manager) reportBudgetStop(ctx context.Context, op, backendName string) {
	telemetry.UsageLimitRejectionsTotal.WithLabelValues(op, "list").Inc()
	m.logger().WarnContext(ctx, op+" stopped at the backend's API usage limit",
		"backend", backendName,
		"detail", "the pass covered only part of the bucket; it resumes on the next run once the budget allows")
}

// Manager orchestrates sync and reconcile passes for one fleet.
type Manager struct {
	backends BackendResolver
	stores   Stores
	usage    UsageRecorder
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
	Codec    StoredInspector
	Log      *slog.Logger
}

// NewManager builds a Manager.
func NewManager(d *Deps) *Manager {
	must.NotNil("d", d)
	must.NotNil("d.Backends", d.Backends)
	must.NotNil("d.Stores", d.Stores)
	must.NotNil("d.Usage", d.Usage)
	return &Manager{backends: d.Backends, stores: d.Stores, usage: d.Usage, codec: d.Codec, log: d.Log}
}

// logger returns the configured logger or the default.
func (m *Manager) logger() *slog.Logger {
	if m.log == nil {
		return slog.Default()
	}
	return m.log
}

// SyncBackend scans a backend's bucket and imports pre-existing objects into
// the ledger. Objects already tracked for the backend are skipped.
// knownBuckets is the full list of configured virtual bucket names; an object
// outside every one of their prefixes is imported at its own key and flagged
// unmanaged, so it counts toward the backend's quota without any worker acting
// on it. Returns counts of imported vs skipped objects.
func (m *Manager) SyncBackend(ctx context.Context, backendName, bucket string, knownBuckets []string) (imported, skipped int, err error) {
	s3b, err := m.resolveLister(backendName)
	if err != nil {
		return 0, 0, err
	}

	// One page of headroom is the entry price, so a pass does not start against
	// a backend that is already spent. The per-page charge inside the walk is
	// what holds it to the limit from there.
	if !m.usage.Allow(backendName, 1, 0, 0) {
		return 0, 0, fmt.Errorf("backend %s: %w", backendName, core.ErrUsageLimitExceeded)
	}

	m.logger().InfoContext(ctx, "starting backend sync", "backend", backendName, "bucket", bucket)

	prefixes := BucketPrefixes(knownBuckets)
	budget := pageBudget{usage: m.usage, backendName: backendName}

	err = s3b.ListObjects(ctx, "", func(objects []backend.ListedObject) error {
		// Charged before the import so the page is paid for even if importing
		// it fails: the listing request reached the provider either way.
		canContinue := budget.charge()
		pImported, pSkipped, pErr := m.importPage(ctx, backendName, prefixes, objects)
		imported += pImported
		skipped += pSkipped
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

// importPage imports one listing page, counting rows that were newly inserted
// against rows the ledger already had.
func (m *Manager) importPage(
	ctx context.Context,
	backendName string,
	bucketPrefixes []string,
	objects []backend.ListedObject,
) (imported, skipped int, err error) {
	for _, obj := range objects {
		unmanaged := Unmanaged(obj.Key, bucketPrefixes)
		outcome, importErr := m.importDiscovered(ctx, &core.ImportObjectRequest{
			Key:       obj.Key,
			Backend:   backendName,
			Size:      obj.SizeBytes,
			Unmanaged: unmanaged,
			WrittenAt: obj.LastModified,
		})
		if importErr != nil {
			return imported, skipped, fmt.Errorf("failed to import %s: %w", obj.Key, importErr)
		}
		switch outcome {
		case core.ImportInserted:
			imported++
		case core.ImportSkippedPendingCleanup:
			// Logged rather than folded silently into the skipped count: a
			// key here is one whose delete never reached the backend, and an
			// operator seeing a run full of them is looking at a cleanup
			// queue that is not draining.
			m.logger().WarnContext(ctx, "skipping key with an outstanding delete",
				"key", obj.Key, "backend", backendName)
			skipped++
		default:
			skipped++
		}
	}
	return imported, skipped, nil
}

// importDiscovered records one key found on a backend, first working out
// whether its bytes are an encryption envelope and, if so, which existing row
// holds the key that reads them. Satisfies ImporterFn, so the sorted-merge
// reconcile and the bulk sync scan classify identically.
func (m *Manager) importDiscovered(ctx context.Context, req *core.ImportObjectRequest) (core.ImportOutcome, error) {
	be, err := m.backends.GetBackend(req.Backend)
	if err != nil {
		return core.ImportSkippedExisting, err
	}
	form, err := ClassifyImport(ctx, ClassifyDeps{
		Backend: be,
		Stores:  m.stores,
		Codec:   m.codec,
		Source:  "reconcile",
		Log:     m.logger(),
	}, req.Backend, req.Key, req.Size)
	if err != nil {
		return core.ImportSkippedExisting, err
	}
	req.Form = form
	return m.stores.ImportObject(ctx, req)
}

// ReconcileBackend reconciles a backend against the ledger using the
// bounded-memory sorted-merge: both sides are walked in byte key order and
// diffed in lockstep, so memory is independent of object count.
//
// The pass is scoped to the backend, not to one virtual bucket: the backend
// client is already pinned to its configured real bucket, and every virtual
// bucket stored there is covered in a single walk. Keys are compared exactly
// as the backend holds them, which is what keeps both streams in the byte
// order the merge requires.
//
// Imports keys present on the backend but absent from the ledger, and deletes
// ledger rows whose keys are no longer on the backend. A key outside every
// configured bucket prefix is imported as unmanaged.
func (m *Manager) ReconcileBackend(ctx context.Context, backendName string, knownBuckets []string) (*Result, error) {
	s3b, err := m.resolveLister(backendName)
	if err != nil {
		return nil, err
	}

	// The same entry price SyncBackend pays, which this path did not ask for at
	// all: a reconcile could start against a backend with nothing left.
	if !m.usage.Allow(backendName, 1, 0, 0) {
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
	return func(ctx context.Context, key, backendName string) error {
		if err := m.stores.DeleteObjectLocation(ctx, key, backendName); err != nil {
			return err
		}
		if _, err := m.stores.SweepStaleCleanupQueueRows(ctx, key, backendName); err != nil {
			m.logger().WarnContext(ctx, "failed to sweep cleanup_queue rows for stale key",
				slog.String("key", key), slog.String("backend", backendName), "error", err)
		}
		return nil
	}
}

// resolveLister unwraps a backend down to the concrete client that can list,
// past any decorators (circuit breaker, metrics) wrapping it.
func (m *Manager) resolveLister(name string) (ObjectLister, error) {
	be, err := m.backends.GetBackend(name)
	if err != nil {
		return nil, err
	}
	inner := be
	for {
		u, ok := inner.(interface{ Unwrap() backend.ObjectBackend })
		if !ok {
			break
		}
		inner = u.Unwrap()
	}
	lister, ok := inner.(ObjectLister)
	if !ok {
		return nil, fmt.Errorf("backend %s does not support listing", name)
	}
	return lister, nil
}

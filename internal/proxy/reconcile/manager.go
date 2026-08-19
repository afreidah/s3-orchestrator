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
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

//go:generate mockgen -destination=mock_manager_test.go -package=reconcile github.com/afreidah/s3-orchestrator/internal/proxy/reconcile Stores,BackendResolver,UsageRecorder

// Stores is the store surface reconciliation needs: import a discovered key,
// drop a stale row, walk the ledger in byte order, and sweep cleanup-queue
// rows belonging to a key that no longer exists.
type Stores interface {
	ImportObject(ctx context.Context, key, backend string, size int64, unmanaged bool, form *core.StoredForm) (bool, error)
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

// UsageRecorder accounts backend API calls against the usage quota, so a
// listing pass shows up in the same counters a client request would.
type UsageRecorder interface {
	APICalls(backendName string, n int64)
}

// Manager orchestrates sync and reconcile passes for one fleet.
type Manager struct {
	backends BackendResolver
	stores   Stores
	usage    UsageRecorder
	log      *slog.Logger
}

// NewManager builds a Manager. log may be nil, in which case the default
// logger is used at call time.
func NewManager(backends BackendResolver, stores Stores, usage UsageRecorder, log *slog.Logger) *Manager {
	return &Manager{backends: backends, stores: stores, usage: usage, log: log}
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

	m.logger().InfoContext(ctx, "starting backend sync", "backend", backendName, "bucket", bucket)

	prefixes := BucketPrefixes(knownBuckets)
	var apiPages int64

	err = s3b.ListObjects(ctx, "", func(objects []backend.ListedObject) error {
		apiPages++
		pImported, pSkipped, pErr := m.importPage(ctx, backendName, prefixes, objects)
		imported += pImported
		skipped += pSkipped
		return pErr
	})

	// Each listing page is one API request against the provider's quota.
	if apiPages > 0 {
		m.usage.APICalls(backendName, apiPages)
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
		inserted, importErr := m.importDiscovered(ctx, obj.Key, backendName, obj.SizeBytes, unmanaged)
		if importErr != nil {
			return imported, skipped, fmt.Errorf("failed to import %s: %w", obj.Key, importErr)
		}
		if inserted {
			imported++
		} else {
			skipped++
		}
	}
	return imported, skipped, nil
}

// importDiscovered records one key found on a backend, first working out
// whether its bytes are an encryption envelope and, if so, which existing row
// holds the key that reads them. Satisfies ImporterFn, so the sorted-merge
// reconcile and the bulk sync scan classify identically.
func (m *Manager) importDiscovered(ctx context.Context, key, backendName string, size int64, unmanaged bool) (bool, error) {
	be, err := m.backends.GetBackend(backendName)
	if err != nil {
		return false, err
	}
	form, err := ClassifyImport(ctx, ClassifyDeps{
		Backend: be,
		Stores:  m.stores,
		Source:  "reconcile",
		Log:     m.logger(),
	}, backendName, key)
	if err != nil {
		return false, err
	}
	return m.stores.ImportObject(ctx, key, backendName, size, unmanaged, form)
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

	var apiPages int64
	s3 := NewS3KeyStream(ctx, s3b, BucketPrefixes(knownBuckets), &apiPages)
	defer s3.Stop()

	dbIter := NewDBCursorStream(DBCursorStreamDeps{Store: m.stores, BackendName: backendName})
	defer dbIter.Stop()

	res := &Result{}
	mergeErr := Sorted(
		ctx, s3, dbIter,
		ImportHandler(m.logger(), backendName, m.importDiscovered, res),
		DeleteHandler(m.logger(), backendName, m.deleter(), res),
	)

	if pages := atomic.LoadInt64(&apiPages); pages > 0 {
		m.usage.APICalls(backendName, pages)
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

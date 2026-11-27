// -------------------------------------------------------------------------------
// Ops - Integrity Operations
//
// Author: Alex Freidah
//
// Verification passes over stored copies: a scrub that re-hashes a batch of
// objects, an on-demand check of every copy of one key, and the backfill that
// computes hashes for objects in hashing buckets stored without one. Alongside
// them, the reconcile of backends against the ledger, the listings of unhashed
// and unreadable copies, and the purge. Scrub, backfill and reconcile each hold
// an advisory lock, so two runs of one never overlap.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// defaultBackfillBatchSize is how many objects one backfill pass hashes when
// the caller asks for no size.
const defaultBackfillBatchSize = 100

// defaultUnreadableBatchSize is how many unreadable copies one purge pass
// reads when the caller asks for no size.
const defaultUnreadableBatchSize = 100

// defaultCopyListLimit is how many copies a listing returns when the caller
// asks for no limit.
const defaultCopyListLimit = 100

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// CopyList reports the copies a listing selects: up to the requested number
// of them, and how many it selects in total.
type CopyList struct {
	Total  int64
	Copies []core.ObjectLocation
}

// IntegrityDeps holds the collaborators Integrity requires. Reconciler is nil
// outside worker mode, which Reconcile reports as unavailable.
type IntegrityDeps struct {
	Scrubber   ScrubberOps
	Reconciler Reconciler
	Store      CopyStore
	Buckets    BucketScopeSource
	Locker     core.AdvisoryLocker
	Config     *config.Live
}

// Integrity serves the verification operations shared by the admin API and
// the web UI.
type Integrity struct {
	d   IntegrityDeps
	log *slog.Logger
}

// NewIntegrity is the explicit-deps constructor.
func NewIntegrity(d *IntegrityDeps) *Integrity {
	must.NotNil("d.Scrubber", d.Scrubber)
	must.NotNil("d.Store", d.Store)
	must.NotNil("d.Buckets", d.Buckets)
	must.NotNil("d.Locker", d.Locker)
	must.NotNil("d.Config", d.Config)
	return &Integrity{d: *d, log: logfmt.Logger(logComponent)}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Scrub runs one verification pass and returns its tally: Attempted counts the
// copies read, Skipped the ones that could not be decoded, and Deferred the
// ones on backends over their read budget. batchSize <= 0 uses the configured
// ScrubberBatchSize. An empty backend verifies copies
// on every backend the read budget allows. observer, when non-nil, receives a
// start and end step per copy verified.
//
// It holds the same advisory lock as the scheduled sweep, so a manual run and
// a tick cannot overlap. If the lock is held it returns ErrScrubInProgress
// instead of waiting.
func (i *Integrity) Scrub(ctx context.Context, batchSize int, backend string, observer progress.Observer) (batch.Summary, error) {
	if batchSize <= 0 {
		batchSize = i.d.Config.Load().Integrity.ScrubberBatchSize
	}

	return underLock(ctx, i.d.Locker, core.LockScrubber, ErrScrubInProgress, func(lockCtx context.Context) (batch.Summary, error) {
		return i.d.Scrubber.Scrub(lockCtx, batchSize, backend, observer)
	})
}

// VerifyKey verifies every recorded copy of one key immediately. Reports
// ErrNotFound when no copy of the key is recorded, which is a different answer
// from a key whose copies all failed verification.
func (i *Integrity) VerifyKey(ctx context.Context, key string) ([]worker.CopyVerification, error) {
	if key == "" {
		return nil, ErrKeyRequired
	}

	copies, err := i.d.Scrubber.ScrubKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(copies) == 0 {
		return nil, ErrNotFound
	}
	return copies, nil
}

// BackfillChecksums computes and stores content hashes for objects that do not
// have one, batchSize objects per pass, pausing for pause between passes to
// rate-limit backend reads. maxObjects <= 0 drains the whole backlog;
// batchSize <= 0 uses the default pass size. An empty backend hashes copies on
// every backend. Only buckets whose new objects are hashed are backfilled.
// observer, when non-nil, receives a start and end step per object hashed.
// Reports ErrBackfillInProgress while another backfill runs, since a second
// one would read every copy again and undo the pause.
func (i *Integrity) BackfillChecksums(ctx context.Context, batchSize, maxObjects int, pause time.Duration, backend string, observer progress.Observer) (worker.BackfillResult, error) {
	if batchSize <= 0 {
		batchSize = defaultBackfillBatchSize
	}

	return underLock(ctx, i.d.Locker, core.LockBackfill, ErrBackfillInProgress, func(lockCtx context.Context) (worker.BackfillResult, error) {
		i.log.InfoContext(lockCtx, "backfill-checksums started",
			"batch_size", batchSize, "max_objects", maxObjects, "pause", pause, "backend", backend)
		return i.d.Scrubber.Backfill(lockCtx, worker.BackfillRequest{
			BatchSize: batchSize, MaxObjects: maxObjects, Pause: pause, Backend: backend,
			Buckets: i.d.Buckets.Wanting(core.StoredHashed),
		}, observer)
	})
}

// Reconcile diffs backendName, or every scannable backend when it is empty,
// against the ledger: it imports the objects the ledger is missing and removes
// the rows whose objects are gone. observer, when non-nil, receives a step per
// backend. It holds the scheduled reconcile's lock, so the two never list a
// backend twice over, and reports ErrReconcileInProgress while that is held.
func (i *Integrity) Reconcile(ctx context.Context, backendName string, observer progress.Observer) (worker.ReconcileResult, error) {
	if i.d.Reconciler == nil {
		return worker.ReconcileResult{}, ErrReconcilerUnavailable
	}
	return underLock(ctx, i.d.Locker, core.LockReconcile, ErrReconcileInProgress, func(lockCtx context.Context) (worker.ReconcileResult, error) {
		return i.d.Reconciler.Reconcile(lockCtx, backendName, observer)
	})
}

// ListUnreadable returns up to limit copies that are encrypted with no key, and
// the total. limit <= 0 uses the default.
func (i *Integrity) ListUnreadable(ctx context.Context, limit int) (CopyList, error) {
	return i.listCopies(ctx, &core.UnreadableCopies, limit, core.LedgerStats.UnreadableCopies)
}

// ListUnhashed returns up to limit copies with no content hash on backend, or
// on every backend when it is empty, and the total. A set managed keeps only
// the managed copies, the ones backfill hashes, or only the unmanaged ones.
// limit <= 0 uses the default.
func (i *Integrity) ListUnhashed(ctx context.Context, backend string, managed *bool, limit int) (CopyList, error) {
	filter := core.UnhashedCopies(backend, managed)
	return i.listCopies(ctx, &filter, limit, func(s core.LedgerStats) int64 {
		return s.UnhashedCount(backend, managed)
	})
}

// PurgeUnreadable discards every copy that is encrypted with no key, batchSize
// at a time, and reports its tally; Succeeded counts the copies purged.
// batchSize <= 0 uses the default.
func (i *Integrity) PurgeUnreadable(ctx context.Context, batchSize int, observer progress.Observer) (batch.Summary, error) {
	if batchSize <= 0 {
		batchSize = defaultUnreadableBatchSize
	}
	return i.d.Scrubber.PurgeUnreadable(ctx, batchSize, observer)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// listCopies returns the first limit copies f selects, with the total the
// ledger counts for them, so the page and the figure beside it come from the
// same definition.
func (i *Integrity) listCopies(ctx context.Context, f *core.FormFilter, limit int, total func(core.LedgerStats) int64) (CopyList, error) {
	if limit <= 0 {
		limit = defaultCopyListLimit
	}
	copies, err := i.d.Store.ListCopiesByForm(ctx, f, limit, core.Cursor{})
	if err != nil {
		return CopyList{}, err
	}
	ledger, err := i.d.Store.LedgerStats(ctx)
	if err != nil {
		return CopyList{}, err
	}
	return CopyList{Total: total(ledger), Copies: copies}, nil
}

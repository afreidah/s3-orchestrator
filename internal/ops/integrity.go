// -------------------------------------------------------------------------------
// Ops - Integrity Operations
//
// Author: Alex Freidah
//
// Verification passes over stored copies: a scrub that re-hashes a batch of
// objects, an on-demand check of every copy of one key, and the backfill that
// computes hashes for objects stored before integrity verification was turned
// on. Each declines with ErrIntegrityDisabled when verification is off.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// defaultBackfillBatchSize is how many objects one backfill pass hashes when
// the caller asks for no size.
const defaultBackfillBatchSize = 100

// defaultUnreadableBatchSize is how many unreadable copies one list or purge
// pass reads when the caller asks for no size.
const defaultUnreadableBatchSize = 100

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// ScrubResult reports one verification pass. Checked counts the copies read;
// the remaining counts partition the ones that did not verify.
type ScrubResult struct {
	Checked    int
	Failed     int
	Unreadable int
	Deferred   int
}

// BackfillResult reports one backfill run. Done is true only when the backlog
// drained; a run stopped by the object cap or a cancelled context reports
// false so the caller knows more work remains. Unreadable counts copies that
// could not be decoded and were skipped.
type BackfillResult struct {
	Processed  int
	Unreadable int
	Done       bool
}

// UnreadableList reports copies that are encrypted with no key: up to the
// requested number of them, and how many exist in total.
type UnreadableList struct {
	Total  int64
	Copies []core.ObjectLocation
}

// PurgeResult reports one purge of unreadable copies.
type PurgeResult struct {
	Purged int
	Failed int
}

// IntegrityDeps holds the collaborators Integrity requires.
type IntegrityDeps struct {
	Scrubber     ScrubberOps
	IntegrityCfg IntegrityConfigLoader
	Locker       AdvisoryLocker
}

// Integrity serves the verification operations shared by the admin API and
// the web UI.
type Integrity struct {
	log          *slog.Logger
	scrubber     ScrubberOps
	integrityCfg IntegrityConfigLoader
	locker       AdvisoryLocker
}

// NewIntegrity is the explicit-deps constructor.
func NewIntegrity(d IntegrityDeps) *Integrity {
	must.NotNil("d.Scrubber", d.Scrubber)
	must.NotNil("d.IntegrityCfg", d.IntegrityCfg)
	must.NotNil("d.Locker", d.Locker)
	return &Integrity{
		log:          slog.Default().With(logfmt.Component("ops")),
		scrubber:     d.Scrubber,
		integrityCfg: d.IntegrityCfg,
		locker:       d.Locker,
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Scrub runs one verification pass and returns the per-pass counts. batchSize
// <= 0 uses the configured ScrubberBatchSize. An empty backend verifies copies
// on every backend the read budget allows. observer, when non-nil, receives a
// start and end step per copy verified.
//
// It holds the same advisory lock as the scheduled sweep, so a manual run and
// a tick cannot overlap. If the lock is held it returns ErrScrubInProgress
// instead of waiting.
func (i *Integrity) Scrub(ctx context.Context, batchSize int, backend string, observer progress.Observer) (ScrubResult, error) {
	icfg := i.integrityCfg.Load()
	if icfg == nil || !icfg.Enabled {
		return ScrubResult{}, ErrIntegrityDisabled
	}
	if batchSize <= 0 {
		batchSize = icfg.ScrubberBatchSize
	}

	var sum worker.WorkSummary
	acquired, err := i.locker.WithAdvisoryLock(ctx, core.LockScrubber, func(lockCtx context.Context) error {
		sum = i.scrubber.Scrub(lockCtx, batchSize, backend, observer)
		return nil
	})
	if err != nil {
		return ScrubResult{}, err
	}
	if !acquired {
		return ScrubResult{}, ErrScrubInProgress
	}

	return ScrubResult{
		Checked:    sum.Attempted,
		Failed:     sum.Failed,
		Unreadable: sum.Skipped,
		Deferred:   sum.Deferred,
	}, nil
}

// VerifyKey verifies every recorded copy of one key immediately. Reports
// ErrNotFound when no copy of the key is recorded, which is a different answer
// from a key whose copies all failed verification.
func (i *Integrity) VerifyKey(ctx context.Context, key string) ([]worker.CopyVerification, error) {
	if key == "" {
		return nil, ErrKeyRequired
	}
	icfg := i.integrityCfg.Load()
	if icfg == nil || !icfg.Enabled {
		return nil, ErrIntegrityDisabled
	}

	copies, err := i.scrubber.ScrubKey(ctx, key)
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
// every backend. observer, when non-nil, receives a start and end step per
// object hashed.
func (i *Integrity) BackfillChecksums(ctx context.Context, batchSize, maxObjects int, pause time.Duration, backend string, observer progress.Observer) (BackfillResult, error) {
	icfg := i.integrityCfg.Load()
	if icfg == nil || !icfg.Enabled {
		return BackfillResult{}, ErrIntegrityDisabled
	}
	if batchSize <= 0 {
		batchSize = defaultBackfillBatchSize
	}

	i.log.InfoContext(ctx, "backfill-checksums started",
		"batch_size", batchSize, "max_objects", maxObjects, "pause", pause, "backend", backend)

	var total int
	done, unreadable := i.drainBackfill(ctx, batchSize, maxObjects, pause, backend, backfillCounter(observer, &total), &total)
	return BackfillResult{Processed: total, Unreadable: unreadable, Done: done}, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// drainBackfill runs backfill passes until the backlog drains, the max-objects
// cap is hit, or the context is cancelled. Reports whether the backlog was
// fully drained and how many copies were skipped as unreadable.
func (i *Integrity) drainBackfill(ctx context.Context, batchSize, maxObjects int, pause time.Duration, backend string, observer progress.Observer, total *int) (done bool, unreadable int) {
	for offset := 0; ; {
		sum, nextOffset := i.scrubber.Backfill(ctx, batchSize, offset, backend, observer)
		unreadable += sum.Skipped
		if nextOffset == 0 {
			return true, unreadable
		}
		offset = nextOffset
		if maxObjects > 0 && *total >= maxObjects {
			return false, unreadable
		}
		if ctx.Err() != nil {
			return false, unreadable
		}
		if pause > 0 && !sleepOrCancel(ctx, pause) {
			return false, unreadable
		}
	}
}

// ListUnreadable returns up to limit copies that are encrypted with no key, and
// the total. limit <= 0 uses the default.
func (i *Integrity) ListUnreadable(ctx context.Context, limit int) (UnreadableList, error) {
	if limit <= 0 {
		limit = defaultUnreadableBatchSize
	}
	copies, total, err := i.scrubber.ListUnreadable(ctx, limit)
	if err != nil {
		return UnreadableList{}, err
	}
	return UnreadableList{Total: total, Copies: copies}, nil
}

// PurgeUnreadable discards every copy that is encrypted with no key, batchSize
// at a time. Stops when a pass purges nothing, so copies that keep failing do
// not loop forever. batchSize <= 0 uses the default.
func (i *Integrity) PurgeUnreadable(ctx context.Context, batchSize int, observer progress.Observer) PurgeResult {
	if batchSize <= 0 {
		batchSize = defaultUnreadableBatchSize
	}
	var res PurgeResult
	for ctx.Err() == nil {
		sum := i.scrubber.PurgeUnreadable(ctx, batchSize, observer)
		res.Purged += sum.Succeeded
		res.Failed += sum.Failed
		if sum.Succeeded == 0 {
			break
		}
	}
	return res
}

// backfillCounter wraps observer so each successfully hashed object bumps
// total, keeping the cumulative count in step with the per-object steps the
// caller renders. The wrapped observer may be nil.
func backfillCounter(observer progress.Observer, total *int) progress.Observer {
	return func(s progress.Step) {
		if s.Phase == progress.PhaseEnd && s.Status == progress.StatusOK {
			*total++
		}
		if observer != nil {
			observer(s)
		}
	}
}

// sleepOrCancel waits for d or for ctx to be cancelled, returning false when
// cancellation wins so the caller stops early.
func sleepOrCancel(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

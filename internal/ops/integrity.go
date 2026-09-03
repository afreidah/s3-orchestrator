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
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// defaultBackfillBatchSize is how many objects one backfill pass hashes when
// the caller asks for no size.
const defaultBackfillBatchSize = 100

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
// false so the caller knows more work remains.
type BackfillResult struct {
	Processed int
	Done      bool
}

// IntegrityDeps holds the collaborators Integrity requires.
type IntegrityDeps struct {
	Scrubber     ScrubberOps
	IntegrityCfg IntegrityConfigLoader
}

// Integrity serves the verification operations shared by the admin API and
// the web UI.
type Integrity struct {
	log          *slog.Logger
	scrubber     ScrubberOps
	integrityCfg IntegrityConfigLoader
}

// NewIntegrity is the explicit-deps constructor.
func NewIntegrity(d IntegrityDeps) *Integrity {
	must.NotNil("d.Scrubber", d.Scrubber)
	must.NotNil("d.IntegrityCfg", d.IntegrityCfg)
	return &Integrity{
		log:          slog.Default().With(logfmt.Component("ops")),
		scrubber:     d.Scrubber,
		integrityCfg: d.IntegrityCfg,
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Scrub runs one verification pass and returns the per-pass counts. batchSize
// <= 0 uses the configured ScrubberBatchSize. observer, when non-nil, receives
// a start and end step per copy verified.
func (i *Integrity) Scrub(ctx context.Context, batchSize int, observer progress.Observer) (ScrubResult, error) {
	icfg := i.integrityCfg.Load()
	if icfg == nil || !icfg.Enabled {
		return ScrubResult{}, ErrIntegrityDisabled
	}
	if batchSize <= 0 {
		batchSize = icfg.ScrubberBatchSize
	}

	sum := i.scrubber.Scrub(ctx, batchSize, observer)
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
// batchSize <= 0 uses the default pass size. observer, when non-nil, receives a
// start and end step per object hashed.
func (i *Integrity) BackfillChecksums(ctx context.Context, batchSize, maxObjects int, pause time.Duration, observer progress.Observer) (BackfillResult, error) {
	icfg := i.integrityCfg.Load()
	if icfg == nil || !icfg.Enabled {
		return BackfillResult{}, ErrIntegrityDisabled
	}
	if batchSize <= 0 {
		batchSize = defaultBackfillBatchSize
	}

	i.log.InfoContext(ctx, "backfill-checksums started",
		"batch_size", batchSize, "max_objects", maxObjects, "pause", pause)

	var total int
	done := i.drainBackfill(ctx, batchSize, maxObjects, pause, backfillCounter(observer, &total), &total)
	return BackfillResult{Processed: total, Done: done}, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// drainBackfill runs backfill passes until the backlog drains, the max-objects
// cap is hit, or the context is cancelled. Returns true only when the backlog
// was fully drained.
func (i *Integrity) drainBackfill(ctx context.Context, batchSize, maxObjects int, pause time.Duration, observer progress.Observer, total *int) bool {
	for offset := 0; ; {
		_, nextOffset := i.scrubber.Backfill(ctx, batchSize, offset, observer)
		if nextOffset == 0 {
			return true
		}
		offset = nextOffset
		if maxObjects > 0 && *total >= maxObjects {
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		if pause > 0 && !sleepOrCancel(ctx, pause) {
			return false
		}
	}
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

// -------------------------------------------------------------------------------
// Ops - Bulk Rewrite Driver
//
// Author: Alex Freidah
//
// The pagination and per-object rewrite every fleet-wide rewrite pass runs.
// Encryption and compression are co-equal consumers: each pass adds or removes
// one stored layer, which decides both the copies it lists and the form it
// leaves them in, and neither service owns the loop.
//
// A rewrite replaces each copy on its own backend with its bytes reformed. The
// listing is in key order, so an object's copies arrive together, and the ones
// that hold the same bytes are rewritten from one read rather than one read
// each. Each new copy lands at a new path and is recorded by the placement
// step, which commits only while the copy read is still the one recorded, and
// the old bytes are deleted after. Readers therefore see either the old copy
// or the new one, never bytes the row does not describe.
//
// A pass reads and rewrites an entire fleet, so it is the largest consumer of
// egress in the system. Each copy is admitted against its backend's usage
// limits as it is rewritten, because a run that fits the budget when it starts
// can exhaust it halfway through. Paging is a batch.Pager cursor walk, shared
// with the other paged passes. Every pass, and key rotation, holds the rewrite
// lock, so two never move the same copies, in the same direction or opposite
// ones.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// bulkRewriteBatchSize is how many locations one listing page of a bulk
// rewrite pass collects, bounding how long any single transaction runs.
const bulkRewriteBatchSize = 100

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// BulkRewriteResult reports one bulk rewrite pass. Objects counts the objects
// the pass rewrote a copy of, which is what a capped pass stops on, and the
// rest count copies. Succeeded, Failed, Skipped and Changed partition Total,
// every copy considered, for a run that completed. Skipped counts copies the
// pass declined on purpose, such as incompressible objects. Changed counts
// copies a client replaced while the pass was rewriting them, meaning a
// conversion overlapped live traffic.
type BulkRewriteResult struct {
	Objects   int
	Succeeded int
	Failed    int
	Skipped   int
	Changed   int
	Total     int
}

// bulkRewriteOp is one direction of the rewrite: the layer it adds (add true)
// or removes, and the labels the run reports under. filter narrows the listing
// further, by backend and, for compression, to worthwhile candidates; the
// driver sets which side of the layer it selects.
//
// onDeclined, when set, records an encoding that missed the ratio so later
// passes can skip the copy; a failure to record costs efficiency, not
// correctness. maxObjects caps how many objects the pass rewrites, or zero for
// the whole listing. It counts objects rewritten rather than objects
// considered, so a capped run converts the number asked for instead of
// spending its budget on copies it declines.
type bulkRewriteOp struct {
	opName      string
	resultLabel string
	counter     *prometheus.CounterVec
	layer       core.StoredFlags
	add         bool
	filter      core.FormFilter
	onDeclined  func(ctx context.Context, loc *core.ObjectLocation, encodedSize int64)
	maxObjects  int
}

// bulkRewriteEnv is what the shared driver needs from whichever service is
// running the pass. Declared so the driver serves the compression passes as
// well as the encryption ones rather than being tied to either.
type bulkRewriteEnv struct {
	log      *slog.Logger
	store    RewriteStore
	rewriter Rewriter
	locker   core.AdvisoryLocker
	layers   *storedform.Layers
}

// rewriteOutcome is what one copy's rewrite attempt produced, ordered from
// best to worst so an object reports the worst of its copies.
type rewriteOutcome int

const (
	rewriteDone rewriteOutcome = iota
	rewriteSkipped
	rewriteChanged
	rewriteErrored
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// selection returns the listing filter: the op's own narrowed to the copies
// still on the wrong side of the layer.
func (op *bulkRewriteOp) selection() core.FormFilter {
	filter := op.filter
	if op.add {
		filter.Without = op.layer
	} else {
		filter.With = op.layer
	}
	return filter
}

// want returns the layers loc should carry once rewritten: its own with the
// pass's layer added or removed.
func (op *bulkRewriteOp) want(loc *core.ObjectLocation) core.StoredFlags {
	if op.add {
		return loc.Form() | op.layer
	}
	return loc.Form() &^ op.layer
}

// status renders one outcome for a progress step, in the vocabulary the other
// streaming passes already use.
func (o rewriteOutcome) status() string {
	switch o {
	case rewriteDone:
		return progress.StatusOK
	case rewriteSkipped, rewriteChanged:
		return progress.StatusSkipped
	default:
		return progress.StatusFailed
	}
}

// count tallies one copy's outcome.
func (r *BulkRewriteResult) count(o rewriteOutcome) {
	r.Total++
	switch o {
	case rewriteDone:
		r.Succeeded++
	case rewriteSkipped:
		r.Skipped++
	case rewriteChanged:
		r.Changed++
	default:
		r.Failed++
	}
}

// run is the shared driver for every bulk rewrite pass, run under the rewrite
// lock. A listing failure stops the run and returns the counts gathered so far
// alongside the error, so a caller can report partial progress. Reports
// ErrRewriteInProgress while another rewrite holds the lock.
//
// obs reports each object as it is processed and may be nil.
func (op *bulkRewriteOp) run(ctx context.Context, env bulkRewriteEnv, obs progress.Observer) (BulkRewriteResult, error) {
	return underLock(ctx, env.locker, core.LockRewrite, ErrRewriteInProgress, func(lockCtx context.Context) (BulkRewriteResult, error) {
		return op.walk(audit.WithWorkload(lockCtx, audit.WorkloadRewrite), env, obs)
	})
}

// walk rewrites every object the listing selects, its listed copies together,
// until the listing is exhausted or the pass reaches its cap. A cancelled pass
// stops before the next object, so the copies it never reached are not
// reported as failed.
func (op *bulkRewriteOp) walk(ctx context.Context, env bulkRewriteEnv, obs progress.Observer) (BulkRewriteResult, error) {
	var res BulkRewriteResult
	filter := op.selection()
	pager := core.CopyPager(env.store, &filter, bulkRewriteBatchSize)
	for copies, err := range core.CopyRuns(pager.Rows(ctx)) {
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			if ctx.Err() == nil {
				env.log.ErrorContext(ctx, op.opName+" list failed", "error", err)
			}
			return res, err
		}
		op.rewriteObject(ctx, env, obs, copies, &res)
		if op.maxObjects > 0 && res.Objects >= op.maxObjects {
			env.log.InfoContext(ctx, op.opName+" reached its limit", "objects", res.Objects,
				op.resultLabel, res.Succeeded, "skipped", res.Skipped, "failed", res.Failed)
			return res, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	env.log.InfoContext(ctx, op.opName+" complete", "objects", res.Objects,
		op.resultLabel, res.Succeeded, "skipped", res.Skipped, "failed", res.Failed, "total", res.Total)
	return res, nil
}

// rewriteObject rewrites one object's listed copies as one progress step,
// reporting the worst of their outcomes, and folds them into res. The copies
// that hold the same bytes are rewritten from one read.
func (op *bulkRewriteOp) rewriteObject(ctx context.Context, env bulkRewriteEnv, obs progress.Observer, copies []core.ObjectLocation, res *BulkRewriteResult) {
	progress.Track(obs, copies[0].ObjectKey, func() string {
		worst, rewrote := rewriteDone, false
		for _, group := range sameBytes(copies) {
			for _, outcome := range op.rewriteGroup(ctx, env, group) {
				res.count(outcome)
				worst = max(worst, outcome)
				rewrote = rewrote || outcome == rewriteDone
			}
		}
		if rewrote {
			res.Objects++
		}
		return worst.status()
	})
}

// rewriteGroup rewrites copies that hold the same bytes and returns each
// copy's outcome, in order.
func (op *bulkRewriteOp) rewriteGroup(ctx context.Context, env bulkRewriteEnv, group []core.ObjectLocation) []rewriteOutcome {
	errs := env.rewriter.RewriteCopies(ctx, &writepath.RewriteRequest{
		Copies: group,
		Layers: env.layers,
		Want:   op.want(&group[0]),
	})
	outcomes := make([]rewriteOutcome, len(group))
	for i, err := range errs {
		outcomes[i] = op.outcome(ctx, env, &group[i], err)
	}
	return outcomes
}

// sameBytes splits one object's listed copies into the groups one read can
// serve: copies carrying the same layers and content hash open to the same
// bytes and end in the same form. A copy with no hash cannot be shown to hold
// what another does, and an unmanaged copy is rewritten from its own bytes,
// so each of those is a group of its own.
func sameBytes(copies []core.ObjectLocation) [][]core.ObjectLocation {
	var groups [][]core.ObjectLocation
	for i := range copies {
		loc := &copies[i]
		at := slices.IndexFunc(groups, func(g []core.ObjectLocation) bool { return holdSameBytes(&g[0], loc) })
		if at < 0 {
			groups = append(groups, []core.ObjectLocation{*loc})
			continue
		}
		groups[at] = append(groups[at], *loc)
	}
	return groups
}

// holdSameBytes reports whether a rewrite of a may stand in for one of b.
func holdSameBytes(a, b *core.ObjectLocation) bool {
	return a.ContentHash != "" && a.ContentHash == b.ContentHash &&
		a.Form() == b.Form() && !a.Unmanaged && !b.Unmanaged
}

// outcome classifies what one copy's rewrite returned: done, declined on
// purpose by the usage limits, the ratio or a backend that stopped taking it,
// overtaken by a client write, or failed.
func (op *bulkRewriteOp) outcome(ctx context.Context, env bulkRewriteEnv, loc *core.ObjectLocation, err error) rewriteOutcome {
	if err == nil {
		op.counter.WithLabelValues("success").Inc()
		return rewriteDone
	}
	if declined, ok := errors.AsType[*storedform.Declined](err); ok {
		if op.onDeclined != nil {
			op.onDeclined(ctx, loc, declined.EncodedSize)
		}
		op.counter.WithLabelValues("skipped").Inc()
		return rewriteSkipped
	}
	switch {
	case errors.Is(err, core.ErrUsageLimitExceeded):
		telemetry.BulkRewriteUsageDeclinedTotal.WithLabelValues(op.opName).Inc()
		return op.skipped(ctx, env, "object declined by usage limits", loc)
	case errors.Is(err, core.ErrTargetRefused):
		return op.skipped(ctx, env, "backend refused the rewritten copy", loc)
	case errors.Is(err, core.ErrCopyChanged):
		return op.changed(ctx, env, loc)
	default:
		return op.failed(ctx, env, "rewrite failed", loc, err)
	}
}

// skipped records one copy the pass declined to rewrite this time.
func (op *bulkRewriteOp) skipped(ctx context.Context, env bulkRewriteEnv, msg string, loc *core.ObjectLocation) rewriteOutcome {
	env.log.WarnContext(ctx, op.opName+": "+msg,
		"key", loc.ObjectKey, "backend", loc.BackendName, "size", loc.SizeBytes)
	op.counter.WithLabelValues("skipped").Inc()
	return rewriteSkipped
}

// failed records one non-fatal rewrite failure.
func (op *bulkRewriteOp) failed(ctx context.Context, env bulkRewriteEnv, msg string, loc *core.ObjectLocation, err error) rewriteOutcome {
	env.log.WarnContext(ctx, op.opName+": "+msg, "key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
	op.counter.WithLabelValues("error").Inc()
	return rewriteErrored
}

// changed records a copy a client replaced while the pass was converting it.
// The placement found a different copy recorded, so nothing was recorded and
// the rewrite deleted the bytes it wrote. It is audited and counted separately
// because it means the pass overlapped live traffic.
func (op *bulkRewriteOp) changed(ctx context.Context, env bulkRewriteEnv, loc *core.ObjectLocation) rewriteOutcome {
	env.log.WarnContext(ctx, op.opName+": copy changed while it was being converted",
		"key", loc.ObjectKey, "backend", loc.BackendName, "storage_key", loc.StorageKey)
	audit.Log(ctx, "storage.ConversionRaced",
		slog.String("operation", op.opName),
		slog.String("key", loc.ObjectKey),
		slog.String("backend", loc.BackendName),
		slog.String("storage_key", loc.StorageKey),
	)
	op.counter.WithLabelValues("changed").Inc()
	telemetry.BulkRewriteCopyChangedTotal.WithLabelValues(op.opName).Inc()
	return rewriteChanged
}

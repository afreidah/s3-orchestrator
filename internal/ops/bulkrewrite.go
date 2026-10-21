// -------------------------------------------------------------------------------
// Ops - Bulk Rewrite Driver
//
// Author: Alex Freidah
//
// The pagination and per-copy move every fleet-wide rewrite pass runs.
// Encryption and compression are co-equal consumers: each supplies a listing
// query, the stored form it wants each copy in, and the labels its run reports
// under, and neither owns the loop.
//
// A rewrite is a move onto the copy's own backend with the bytes reformed on
// the way. It lands at a new path and is recorded by the placement step, which
// commits only while the copy read is still the one recorded, and the old bytes
// are deleted after. Readers therefore see either the old copy or the new one,
// never bytes the row does not describe.
//
// A pass reads and rewrites an entire fleet, so it is the largest consumer of
// egress in the system. Each copy is admitted against its backend's usage
// limits as it is moved, because a run that fits the budget when it starts can
// exhaust it halfway through. Paging is a batch.Pager cursor walk, shared with
// the other paged passes.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"errors"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
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

// BulkRewriteResult reports one bulk rewrite pass. Total counts every copy
// considered, so Total - Succeeded - Failed - Skipped is zero for a run that
// completed. Skipped counts copies the pass declined on purpose, such as
// incompressible objects. Changed counts copies a client replaced while the
// pass was rewriting them, meaning a conversion overlapped live traffic.
type BulkRewriteResult struct {
	Succeeded int
	Failed    int
	Skipped   int
	Changed   int
	Total     int
}

// bulkRewriteOp parameterises one direction of the rewrite: the listing query,
// the stored form each copy should end up in, and the labels the run reports
// under.
type bulkRewriteOp struct {
	opName      string
	resultLabel string
	counter     *prometheus.CounterVec
	listFn      func(ctx context.Context, batchSize int, after core.Cursor) ([]core.ObjectLocation, error)
	// want names the layers a copy should carry once rewritten.
	want func(loc *core.ObjectLocation) core.StoredFlags
	// onDeclined, when set, records an encoding that missed the ratio so later
	// passes can skip the copy. A failure to record costs efficiency, not
	// correctness.
	onDeclined func(ctx context.Context, loc *core.ObjectLocation, encodedSize int64)
	// maxRewrites caps how many copies the pass rewrites before stopping, or zero
	// for the whole listing. It counts rewrites rather than rows considered, so a
	// capped run converts the number asked for instead of spending its budget
	// on copies it declines.
	maxRewrites int
}

// bulkRewriteEnv is what the shared driver needs from whichever service is
// running the pass. Declared so the driver serves the compression passes as
// well as the encryption ones rather than being tied to either.
type bulkRewriteEnv struct {
	log     *slog.Logger
	runtime RuntimeOps
	mover   Mover
	layers  *storedform.Layers
}

// rewriteOutcome is what one object's rewrite attempt produced.
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

// item maps one outcome onto the shared tally. A changed copy was declined, so
// it tallies as skipped.
func (o rewriteOutcome) item() batch.ItemOutcome {
	switch o {
	case rewriteDone:
		return batch.ItemSucceeded
	case rewriteSkipped, rewriteChanged:
		return batch.ItemSkipped
	default:
		return batch.ItemFailed
	}
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

// run is the shared driver for every bulk rewrite pass. A listing
// failure stops the run and returns the counts gathered so far alongside the
// error, so a caller can report partial progress.
//
// obs reports each object as it is processed and may be nil.
func (op bulkRewriteOp) run(ctx context.Context, env bulkRewriteEnv, obs progress.Observer) (BulkRewriteResult, error) {
	ctx = audit.WithWorkload(ctx, audit.WorkloadRewrite)
	var res BulkRewriteResult
	pager := batch.Pager[core.ObjectLocation, core.Cursor]{
		// A capped run's last page is asked for short, so the walk compares
		// each page against what was asked for rather than the batch constant.
		PageSize: func() int { return bulkRewritePageSize(op.maxRewrites, res.Succeeded) },
		List: func(ctx context.Context, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
			rows, err := op.listFn(ctx, limit, after)
			if err != nil {
				env.log.ErrorContext(ctx, op.opName+" list failed", "error", err)
			}
			return rows, err
		},
		CursorOf: func(row core.ObjectLocation) core.Cursor {
			return core.Cursor{ObjectKey: row.ObjectKey, BackendName: row.BackendName}
		},
	}
	stop, err := pager.Walk(ctx, func(ctx context.Context, rows []core.ObjectLocation) (batch.Step, error) {
		capped, err := op.runPage(ctx, env, obs, rows, &res)
		return batch.Step{Stop: capped}, err
	})
	if err != nil {
		return res, err
	}
	if stop == batch.Stopped {
		env.log.InfoContext(ctx, op.opName+" reached its limit",
			op.resultLabel, res.Succeeded, "skipped", res.Skipped, "failed", res.Failed)
		return res, nil
	}

	env.log.InfoContext(ctx, op.opName+" complete",
		op.resultLabel, res.Succeeded, "skipped", res.Skipped, "failed", res.Failed, "total", res.Total)
	return res, nil
}

// runPage rewrites one listing page and folds its outcomes into res. A
// cancelled pass stops dispatching rows, so the copies it never reached are not
// reported as failed downloads. A changed copy is tallied as skipped by the
// runner and counted apart here. Reports whether the pass reached its cap; the
// page was asked for no more rows than the cap had room for, so the cap is
// checked once the page is done.
func (op bulkRewriteOp) runPage(ctx context.Context, env bulkRewriteEnv, obs progress.Observer, rows []core.ObjectLocation, res *BulkRewriteResult) (bool, error) {
	changed := 0
	runner := batch.Runner[core.ObjectLocation]{
		Name:        op.opName,
		Concurrency: 1,
		Observer:    obs,
		Key:         func(row core.ObjectLocation) string { return row.ObjectKey },
	}
	sum := runner.Run(ctx, rows, func(ctx context.Context, row core.ObjectLocation) batch.ItemResult {
		outcome := op.processLocation(ctx, env, &row)
		if outcome == rewriteChanged {
			changed++
		}
		return batch.ItemResult{Outcome: outcome.item(), Status: outcome.status()}
	})
	res.Succeeded += sum.Succeeded
	res.Failed += sum.Failed
	res.Changed += changed
	res.Skipped += sum.Skipped - changed
	res.Total += sum.Attempted + sum.Skipped
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return op.maxRewrites > 0 && res.Succeeded >= op.maxRewrites, nil
}

// bulkRewritePageSize is how many rows to ask for next: a full page, or only
// what a capped run still has room for.
func bulkRewritePageSize(maxRewrites, rewritten int) int {
	if maxRewrites <= 0 {
		return bulkRewriteBatchSize
	}
	if remaining := maxRewrites - rewritten; remaining < bulkRewriteBatchSize {
		return remaining
	}
	return bulkRewriteBatchSize
}

// processLocation rewrites one copy by moving it onto its own backend in the
// form the pass wants. Failures are logged and counted rather than returned,
// so one bad object does not end the pass.
func (op bulkRewriteOp) processLocation(ctx context.Context, env bulkRewriteEnv, loc *core.ObjectLocation) rewriteOutcome {
	be, err := env.runtime.GetBackend(loc.BackendName)
	if err != nil {
		return op.failed(ctx, env, "backend not found", loc, err)
	}
	want := op.want(loc)
	_, err = env.mover.MoveObject(ctx, &writepath.MoveRequest{
		Source:      loc,
		SrcBackend:  be,
		DestBackend: be,
		DestName:    loc.BackendName,
		Profile:     writepath.RewriteMove,
		Reform: func(ctx context.Context, src *s3be.GetObjectResult) (*storedform.Reformed, error) {
			return env.layers.Reform(ctx, src.Body, src.Size, &loc.StoredForm, want)
		},
	})
	return op.outcome(ctx, env, loc, err)
}

// outcome classifies what one move returned: done, declined on purpose by the
// usage limits, the ratio or a backend that stopped taking it, overtaken by a
// client write, or failed.
func (op bulkRewriteOp) outcome(ctx context.Context, env bulkRewriteEnv, loc *core.ObjectLocation, err error) rewriteOutcome {
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
func (op bulkRewriteOp) skipped(ctx context.Context, env bulkRewriteEnv, msg string, loc *core.ObjectLocation) rewriteOutcome {
	env.log.WarnContext(ctx, op.opName+": "+msg,
		"key", loc.ObjectKey, "backend", loc.BackendName, "size", loc.SizeBytes)
	op.counter.WithLabelValues("skipped").Inc()
	return rewriteSkipped
}

// failed records one non-fatal rewrite failure.
func (op bulkRewriteOp) failed(ctx context.Context, env bulkRewriteEnv, msg string, loc *core.ObjectLocation, err error) rewriteOutcome {
	env.log.WarnContext(ctx, op.opName+": "+msg, "key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
	op.counter.WithLabelValues("error").Inc()
	return rewriteErrored
}

// changed records a copy a client replaced while the pass was converting it.
// The placement found a different copy recorded, so nothing was recorded and
// the move deleted the bytes it wrote. It is audited and counted separately
// because it means the pass overlapped live traffic.
func (op bulkRewriteOp) changed(ctx context.Context, env bulkRewriteEnv, loc *core.ObjectLocation) rewriteOutcome {
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

// -------------------------------------------------------------------------------
// Ops - Bulk Rewrite Driver
//
// Author: Alex Freidah
//
// The pagination, download, transform, re-upload and metadata-update loop every
// fleet-wide rewrite pass runs. Encryption and compression are co-equal
// consumers: each supplies a listing query, a transform and the labels its run
// reports under, and neither owns the loop.
//
// A pass reads and rewrites an entire fleet, so it is the largest consumer of
// egress in the system. Everything here that looks defensive - per-object usage
// admission, cursor paging, the cap counting rewrites rather than rows - is
// there because a pass that gets one of them wrong either exhausts a metered
// backend or silently skips half the fleet while reporting success.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// bulkRewriteBatchSize is how many locations one listing page of a bulk
// rewrite pass collects, bounding how long any single transaction runs.
const bulkRewriteBatchSize = 100

// BulkRewriteResult reports one bulk rewrite pass. Total counts every copy
// considered, so Total - Succeeded - Failed - Skipped is zero for a run that
// completed.
//
// Skipped is separate from Failed because a copy can be left alone on purpose:
// a compression pass declines objects too small or too incompressible to be
// worth encoding, and reporting those as failures would make a healthy run look
// broken.
type BulkRewriteResult struct {
	Succeeded int
	Failed    int
	Skipped   int
	Total     int
}

// bulkRewriteRow is the minimum surface every row passed to the bulk-rewrite
// driver must expose. The encryption and compression row wrappers satisfy it.
type bulkRewriteRow interface {
	rewriteKey() string
	rewriteBackend() string
	rewriteSize() int64
}

// bulkRewriteOp parameterises one direction of the rewrite: the listing query,
// the transform, and the labels the run reports under.
type bulkRewriteOp[L bulkRewriteRow] struct {
	opName      string
	resultLabel string
	counter     *prometheus.CounterVec
	listFn      func(ctx context.Context, batchSize int, after core.Cursor) ([]L, error)
	// declines reports a row the pass will not rewrite, before anything is
	// downloaded for it. Only what the row itself says can be judged here; a
	// question about the bytes has to wait for the transform. Nil means every
	// listed row is attempted.
	declines func(loc L) bool
	// rewrite consumes a downloaded object and produces what to upload in its
	// place. Implementations close the source body if they fail before
	// returning. Returning errSkipRewrite leaves the object exactly as it is
	// and counts it as skipped.
	rewrite func(ctx context.Context, src *s3be.GetObjectResult, loc L) (rewritten, error)
	// maxRewrites caps how many copies the pass rewrites before stopping, or zero
	// for the whole listing. It counts rewrites rather than rows considered, so a
	// capped run converts the number asked for instead of spending its budget
	// on copies it declines.
	maxRewrites int
}

// rewritten is one transformed object: the bytes to upload, the size to declare
// for them, the metadata update to run once they are durable, and the release
// for anything the transform had to buffer.
//
// release is nil for a transform that streams. It runs whatever the upload
// does, because a pass that leaks a buffer per failed object is a pass that
// fills a disk partway through a fleet.
type rewritten struct {
	body    io.Reader
	size    int64
	commit  func() error
	release func()
}

// errSkipRewrite reports an object a pass declines to rewrite, as opposed to
// one it tried and could not. Only the transform can raise it, because whether
// a rewrite is worth doing is a question about the object's own bytes.
var errSkipRewrite = errors.New("rewrite declined")

// bulkRewriteEnv is what the shared driver needs from whichever service is
// running the pass. Declared so the driver serves the compression passes as
// well as the encryption ones rather than being tied to either.
type bulkRewriteEnv struct {
	log     *slog.Logger
	runtime RuntimeOps
	usage   UsageGate
}

// rewriteOutcome is what one object's rewrite attempt produced.
type rewriteOutcome int

const (
	rewriteDone rewriteOutcome = iota
	rewriteSkipped
	rewriteErrored
)

// status renders one outcome for a progress step, in the vocabulary the other
// streaming passes already use.
func (o rewriteOutcome) status() string {
	switch o {
	case rewriteDone:
		return progress.StatusOK
	case rewriteSkipped:
		return progress.StatusSkipped
	default:
		return progress.StatusFailed
	}
}

// run is the shared driver for every bulk rewrite pass. A listing
// failure stops the run and returns the counts gathered so far alongside the
// error, so a caller can report partial progress.
//
// obs reports each object as it is processed and may be nil, which is what a
// caller wanting only the summary passes. These passes read and rewrite every
// object in a fleet, so a caller watching one needs to see it move rather than
// wait on a spinner.
//
// Paging is by cursor, and it has to be: a rewritten object stops matching the
// listing that selected it, so the set shrinks as the pass walks it. An offset
// advanced against that steps clean over the rows that moved up to fill the
// gap - a full fleet decompress skips every other page and stops early, having
// reported success. The cursor names the last row seen, so rows leaving the set
// behind it move nothing.
func (op bulkRewriteOp[L]) run(ctx context.Context, env bulkRewriteEnv, obs progress.Observer) (BulkRewriteResult, error) {
	var res BulkRewriteResult
	var after core.Cursor
	for {
		pageSize := bulkRewritePageSize(op.maxRewrites, res.Succeeded)
		rows, err := op.listFn(ctx, pageSize, after)
		if err != nil {
			env.log.ErrorContext(ctx, op.opName+" list failed", "error", err)
			return res, err
		}
		if len(rows) == 0 {
			break
		}

		stop, err := op.runPage(ctx, env, obs, rows, &res, &after)
		if err != nil {
			return res, err
		}
		if stop {
			env.log.InfoContext(ctx, op.opName+" reached its limit",
				op.resultLabel, res.Succeeded, "skipped", res.Skipped, "failed", res.Failed)
			return res, nil
		}

		// Compared against what was asked for, not the constant: a capped run's
		// last page is short by design, and reading that as an exhausted listing
		// would end a pass that still had room.
		if len(rows) < pageSize {
			break
		}
	}

	env.log.InfoContext(ctx, op.opName+" complete",
		op.resultLabel, res.Succeeded, "skipped", res.Skipped, "failed", res.Failed, "total", res.Total)
	return res, nil
}

// runPage processes one listing page, advancing res and the cursor.
// Reports whether the pass should stop because it reached its cap.
func (op bulkRewriteOp[L]) runPage(ctx context.Context, env bulkRewriteEnv, obs progress.Observer, rows []L, res *BulkRewriteResult, after *core.Cursor) (bool, error) {
	for _, row := range rows {
		// Checked per row rather than per page: without it a cancelled pass
		// runs out the rest of the page, failing every remaining copy's
		// download and reporting a hundred failures that never happened.
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		res.Total++
		outcome := rewriteErrored
		progress.Track(obs, row.rewriteKey(), func() string {
			outcome = op.processLocation(ctx, env, row)
			return outcome.status()
		})
		res.tally(outcome)
		*after = core.Cursor{ObjectKey: row.rewriteKey(), BackendName: row.rewriteBackend()}
		if op.maxRewrites > 0 && res.Succeeded >= op.maxRewrites {
			return true, nil
		}
	}
	return false, nil
}

// tally records one copy's outcome against the running counts.
func (r *BulkRewriteResult) tally(outcome rewriteOutcome) {
	switch outcome {
	case rewriteDone:
		r.Succeeded++
	case rewriteSkipped:
		r.Skipped++
	case rewriteErrored:
		r.Failed++
	}
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

// processLocation runs one rewrite step for a single location: admit the
// work against the backend's usage limits, download, transform, re-upload,
// account for what it spent, then update metadata. Failures are logged and
// counted rather than returned, so one bad object does not end the pass.
//
// A pass reads and rewrites an entire fleet, so it is the largest consumer of
// egress in the system and the one most able to exhaust a metered backend. It
// is admitted per object rather than once per run because the budget is spent
// as it goes: a run that fits when it starts can stop fitting halfway through.
func (op bulkRewriteOp[L]) processLocation(ctx context.Context, env bulkRewriteEnv, loc L) rewriteOutcome {
	key, backendName, sizeBytes := loc.rewriteKey(), loc.rewriteBackend(), loc.rewriteSize()

	if op.declines != nil && op.declines(loc) {
		op.counter.WithLabelValues("skipped").Inc()
		return rewriteSkipped
	}

	// The read is charged at the row's size and the write at the same figure
	// as an estimate, since the transform's output is not known yet. Both are
	// re-charged with the real numbers once they are.
	if !env.usage.WithinLimits(backendName, 2, sizeBytes, sizeBytes) {
		op.counter.WithLabelValues("skipped").Inc()
		telemetry.BulkRewriteUsageDeclinedTotal.WithLabelValues(op.opName).Inc()
		env.log.WarnContext(ctx, op.opName+": object declined by usage limits",
			"key", key, "backend", backendName, "size", sizeBytes)
		return rewriteSkipped
	}

	be, err := env.runtime.GetBackend(backendName)
	if err != nil {
		return op.failed(ctx, env, "backend not found", key, backendName, err)
	}

	src, err := be.GetObject(ctx, key, "")
	if err != nil {
		env.usage.Record(backendName, 1, 0, 0)
		return op.failed(ctx, env, "download failed", key, backendName, err)
	}
	env.usage.Record(backendName, 1, src.Size, 0)

	out, err := op.rewrite(ctx, src, loc)
	if err != nil {
		src.Body.Close()
		if errors.Is(err, errSkipRewrite) {
			op.counter.WithLabelValues("skipped").Inc()
			return rewriteSkipped
		}
		return op.failed(ctx, env, "transform failed", key, backendName, err)
	}
	if out.release != nil {
		defer out.release()
	}

	_, err = be.PutObject(ctx, key, out.body, out.size, src.ContentType, src.Metadata)
	src.Body.Close()
	if err != nil {
		env.usage.Record(backendName, 1, 0, 0)
		return op.failed(ctx, env, "re-upload failed", key, backendName, err)
	}
	env.usage.Record(backendName, 1, 0, out.size)

	if err := out.commit(); err != nil {
		return op.failed(ctx, env, "metadata update failed", key, backendName, err)
	}

	op.counter.WithLabelValues("success").Inc()
	return rewriteDone
}

// failed records one non-fatal rewrite failure.
func (op bulkRewriteOp[L]) failed(ctx context.Context, env bulkRewriteEnv, msg, key, backendName string, err error) rewriteOutcome {
	env.log.WarnContext(ctx, op.opName+": "+msg, "key", key, "backend", backendName, "error", err)
	op.counter.WithLabelValues("error").Inc()
	return rewriteErrored
}

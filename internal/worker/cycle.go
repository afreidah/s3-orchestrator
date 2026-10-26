// -------------------------------------------------------------------------------
// Worker Cycle Envelope - Shared audit, tracing and cycle reporting
//
// Author: Alex Freidah
//
// Every worker pass runs inside its workload, and the workload's name is the
// one name the worker goes by: its service, span, admission label, batch log
// line and cycle metrics. runOpsCycle is the envelope for the passes that
// return a summary and an error, runTickCycle for the queue passes that tally
// their own failures and cannot fail. Both record how the pass ended, so no
// worker reports its own cycles.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// cycleResult is what a worker pass returns: a tally the envelope classifies,
// and the attributes its completion audit line carries. batch.Summary is one,
// and a summary embedding it adds its own counts by overriding AuditAttrs.
type cycleResult interface {
	Outcome() string
	AuditAttrs() []slog.Attr
}

// workerLogger returns a worker's logger, its component named for the
// workload, so its lines carry the same name as its service and metrics.
func workerLogger(wl audit.Workload) *slog.Logger {
	return tickrunner.ComponentLogger(string(wl))
}

// runOps attaches a fresh request ID and the workload, opens a span named op,
// and invokes fn inside them. It records nothing; runOpsCycle does that for a
// pass.
func runOps[T any](ctx context.Context, wl audit.Workload, op string, fn func(ctx context.Context) (T, error)) (T, error) {
	ctx = audit.WithWorkload(audit.WithRequestID(ctx, audit.NewID()), wl)
	return observe.Run(ctx, cycleOp(op), fn)
}

// runOpsCycle runs one pass of a worker under runOps, named for its workload,
// and records how the pass ended.
func runOpsCycle[T cycleResult](ctx context.Context, wl audit.Workload, fn func(ctx context.Context) (T, error)) (T, error) {
	return runOps(ctx, wl, string(wl), func(ctx context.Context) (T, error) {
		start := time.Now()
		res, err := fn(ctx)
		recordCycle(ctx, wl, start, res, err)
		return res, err
	})
}

// runTickCycle runs a queue pass that cannot fail under the workload and its
// span, and records how it ended. It does not mint a request ID: the tick that
// calls it already has one.
func runTickCycle[T cycleResult](ctx context.Context, wl audit.Workload, fn func(ctx context.Context) T) T {
	return observe.RunValue(audit.WithWorkload(ctx, wl), cycleOp(string(wl)), func(ctx context.Context) T {
		start := time.Now()
		res := fn(ctx)
		recordCycle(ctx, wl, start, res, nil)
		return res
	})
}

// recordCycle counts the pass under its outcome, times it, and writes the
// "<workload>.complete" audit line for a pass that did something. A pass that
// failed before it had a tally reports the error outcome.
func recordCycle(ctx context.Context, wl audit.Workload, start time.Time, res cycleResult, err error) {
	elapsed := time.Since(start)
	outcome := batch.OutcomeError
	if err == nil {
		outcome = res.Outcome()
	}
	telemetry.WorkerCyclesTotal.WithLabelValues(string(wl), outcome).Inc()
	telemetry.WorkerCycleDurationSeconds.WithLabelValues(string(wl)).Observe(elapsed.Seconds())
	if outcome == batch.OutcomeEmpty {
		return
	}
	attrs := append(res.AuditAttrs(), slog.String("outcome", outcome), slog.Duration("duration", elapsed))
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	audit.Log(ctx, string(wl)+".complete", attrs...)
}

// cycleOp describes the span the envelopes open, named op and tagged with it as
// the operation that lands on metrics and audit logs.
func cycleOp(op string) observe.Op {
	return observe.Internal(op,
		[]attribute.KeyValue{telemetry.AttrOperation.String(op)},
		nil)
}

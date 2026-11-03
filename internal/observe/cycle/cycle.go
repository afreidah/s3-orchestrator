// -------------------------------------------------------------------------------
// Cycle - The Envelope Every Background Pass Runs In
//
// Author: Alex Freidah
//
// A pass is named by its workload, and that name is the one it goes by in its
// span, its usage charges, its cycle metrics and its completion audit entry.
// A pass with nothing to do counts in the metrics and writes nothing else, so
// a worker polling an empty queue stays quiet.
// -------------------------------------------------------------------------------

package cycle

import (
	"cmp"
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Result is what a pass returns. Outcome labels the cycle metric, and
// AuditAttrs are the completion entry's attributes. Reported says the pass
// had work worth an entry, and Changed says it changed what backends hold.
// batch.Summary is one, and a summary embedding it overrides what it adds.
type Result interface {
	Outcome() string
	AuditAttrs() []slog.Attr
	Reported() bool
	Changed() bool
}

// Refresher pushes fresh quota gauges after a pass that changed what backends
// hold.
type Refresher interface {
	UpdateQuotaMetrics(ctx context.Context) error
}

// Pass names a pass and what runs after it. Span defaults to the workload's
// name. Refresh, when set, is called after a pass whose result Changed.
type Pass struct {
	Workload audit.Workload
	Span     string
	Refresh  Refresher
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Run runs fn as one pass and reports it. The pass keeps the caller's request
// ID, so a pass an admin request started is traced to that request, and gets
// one of its own when the caller has none.
func Run[T Result](ctx context.Context, p Pass, fn func(ctx context.Context) (T, error)) (T, error) {
	if audit.RequestID(ctx) == "" {
		ctx = audit.WithRequestID(ctx, audit.NewID())
	}
	ctx = audit.WithWorkload(ctx, p.Workload)
	name := cmp.Or(p.Span, string(p.Workload))
	op := observe.Internal(name, []attribute.KeyValue{telemetry.AttrOperation.String(name)}, nil)
	return observe.Run(ctx, op, func(ctx context.Context) (T, error) {
		start := time.Now()
		res, err := fn(ctx)
		p.report(ctx, time.Since(start), res, err)
		return res, err
	})
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// report counts the pass under its outcome, refreshes the quota gauges when it
// changed anything, and writes the "<workload>.complete" entry when it failed
// or had work. A pass that failed before it had a tally reports the error
// outcome.
func (p Pass) report(ctx context.Context, elapsed time.Duration, res Result, err error) {
	wl := string(p.Workload)
	outcome := batch.OutcomeError
	if err == nil {
		outcome = res.Outcome()
	}
	telemetry.WorkerCyclesTotal.WithLabelValues(wl, outcome).Inc()
	telemetry.WorkerCycleDurationSeconds.WithLabelValues(wl).Observe(elapsed.Seconds())
	if p.Refresh != nil && res.Changed() {
		if rerr := p.Refresh.UpdateQuotaMetrics(ctx); rerr != nil {
			slog.Default().With(logfmt.Component(wl)).WarnContext(ctx, "quota metrics refresh failed after pass", "error", rerr)
		}
	}
	if err == nil && !res.Reported() {
		return
	}
	attrs := append(res.AuditAttrs(), slog.String("outcome", outcome), slog.Duration("duration", elapsed))
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	audit.Log(ctx, wl+".complete", attrs...)
}

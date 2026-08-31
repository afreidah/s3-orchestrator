// -------------------------------------------------------------------------------
// Span Helper  -  uniform tracing + metrics + status emission
//
// Author: Alex Freidah
//
// Run wraps a unit of work with the four cross-cutting concerns that every
// instrumented method previously hand-rolled: span lifecycle, success/error
// status, OpenTelemetry RecordError, and operation-metrics emission. Call
// sites build an Op describing the trace span (name, kind, attrs) and an
// optional Recorder for Prometheus, then hand a closure to Run.
//
// The helper enforces invariants the hand-written boilerplate often missed:
//   - span.End() always fires (defer).
//   - SetStatus(Ok) on success and SetStatus(Error) + RecordError on failure
//     fire uniformly; previous code only set Ok at some sites.
//   - The recorder receives the wall-clock duration via the captured start.
//
// Usage:
//
//	return observe.Run(ctx,
//	    observe.Client("s3.PutObject", attrs, b.recordOperation),
//	    func(ctx context.Context) (string, error) { ... })
// -------------------------------------------------------------------------------

// Package observe provides cross-cutting observability helpers (spans,
// metrics, status) that wrap individual operations without leaking the
// boilerplate into every method body.
package observe

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// Recorder receives operation timing and outcome for Prometheus or other
// metric backends. It is invoked exactly once per Run, after the underlying
// closure returns. A nil recorder is allowed  -  Run skips metric emission
// when none is configured.
type Recorder func(operation string, start time.Time, err error)

// Op describes a single observable operation. Build one with Client, Server,
// or Internal; pass it to Run.
type Op struct {
	Name     string
	Kind     trace.SpanKind
	Attrs    []attribute.KeyValue
	Recorder Recorder
}

// Client returns an Op for an outbound call (e.g., backend S3 request).
func Client(name string, attrs []attribute.KeyValue, recorder Recorder) Op {
	return Op{Name: name, Kind: trace.SpanKindClient, Attrs: attrs, Recorder: recorder}
}

// Internal returns an Op for in-process work (worker passes, internal helpers).
func Internal(name string, attrs []attribute.KeyValue, recorder Recorder) Op {
	return Op{Name: name, Kind: trace.SpanKindInternal, Attrs: attrs, Recorder: recorder}
}

// Run executes fn under a tracing span, records operation metrics via
// op.Recorder when present, and emits status codes uniformly. The span and
// recorder fire even if fn panics (panic is re-thrown after cleanup).
func Run[R any](ctx context.Context, op Op, fn func(context.Context) (R, error)) (R, error) {
	start := time.Now()
	ctx, span := telemetry.Tracer().Start(ctx, op.Name,
		trace.WithSpanKind(op.Kind),
		trace.WithAttributes(op.Attrs...),
	)
	defer span.End()

	var result R
	var err error
	defer func() {
		if op.Recorder != nil {
			op.Recorder(op.Name, start, err)
		}
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
		} else {
			span.SetStatus(codes.Ok, "")
		}
	}()

	result, err = fn(ctx)
	return result, err
}

// RunErr is the void variant of Run for operations that return only an
// error.
func RunErr(ctx context.Context, op Op, fn func(context.Context) error) error {
	_, err := Run(ctx, op, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	})
	return err
}

// RunValue is the variant for work that reports a result but cannot fail -
// the background passes that tally their own outcomes and return a summary
// rather than an error. The span always reports success, because the work has
// no failure to report; anything that went wrong inside is in the summary.
func RunValue[R any](ctx context.Context, op Op, fn func(context.Context) R) R {
	result, _ := Run(ctx, op, func(ctx context.Context) (R, error) {
		return fn(ctx), nil
	})
	return result
}

// RecordSpanError marks the span as failed with err and records the error
// as a span event. Use this at every error return path in hand-rolled
// span scopes so the trace consistently carries both the status and the
// error event.
func RecordSpanError(span trace.Span, err error) {
	span.SetStatus(codes.Error, err.Error())
	span.RecordError(err)
}

// MarkSpanError marks the span as failed with a literal message. Use
// when the failure has no err value to record - for example, a sentinel
// branch that returns early before any error is constructed.
func MarkSpanError(span trace.Span, msg string) {
	span.SetStatus(codes.Error, msg)
}

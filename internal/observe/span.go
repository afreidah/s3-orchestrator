// -------------------------------------------------------------------------------
// Span Helper - Tracing and Operation Metrics Around One Call
//
// Author: Alex Freidah
//
// Run wraps a unit of work in a trace span and an optional Recorder for its
// operation metric. The span always ends, its status follows the error the
// work returns, and a panic is recorded as a failure before it propagates.
// -------------------------------------------------------------------------------

package observe

import (
	"context"
	"errors"
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

// Op describes a single observable operation. Build one with Client or
// Internal; pass it to Run.
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

// errPanicked is the error a span and recorder see for work that panicked.
var errPanicked = errors.New("panicked")

// Run executes fn under a tracing span, records operation metrics via
// op.Recorder when present, and emits status codes uniformly. If fn panics,
// both see errPanicked and the panic continues unchanged.
func Run[R any](ctx context.Context, op Op, fn func(context.Context) (R, error)) (R, error) {
	start := time.Now()
	ctx, span := telemetry.Tracer().Start(ctx, op.Name,
		trace.WithSpanKind(op.Kind),
		trace.WithAttributes(op.Attrs...),
	)
	defer span.End()

	var result R
	var err error
	returned := false
	defer func() {
		if !returned {
			err = errPanicked
		}
		if op.Recorder != nil {
			op.Recorder(op.Name, start, err)
		}
		if err != nil {
			RecordSpanError(span, err)
		} else {
			span.SetStatus(codes.Ok, "")
		}
	}()

	result, err = fn(ctx)
	returned = true
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

// RecordSpanError sets the span's error status and records err as an event.
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

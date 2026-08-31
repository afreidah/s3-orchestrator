// -------------------------------------------------------------------------------
// Worker Cycle Envelope - Shared audit + tracing scaffolding
//
// Author: Alex Freidah
//
// runOpsCycle wraps the audit.WithRequestID + observe.Run + observe.Internal
// envelope every operator-driven worker cycle uses, for the entry points that
// return a count and an error. runTickCycle is the same envelope for the
// periodic scans that tally their own outcomes into a summary and cannot fail.
// -------------------------------------------------------------------------------

package worker

import (
	"context"

	"go.opentelemetry.io/otel/attribute"

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// runOpsCycle attaches a fresh request ID, opens an internal-operation span,
// and invokes fn inside both. Returns whatever fn returns.
func runOpsCycle[T any](ctx context.Context, spanName, opAttr string, fn func(ctx context.Context) (T, error)) (T, error) {
	ctx = audit.WithRequestID(ctx, audit.NewID())
	return observe.Run(ctx, cycleOp(spanName, opAttr), fn)
}

// runTickCycle opens the same span for a periodic scan that returns a summary
// and no error. It does not mint a request ID: a tick is not an operator
// action, and the workers that run one already bracket their own audit scope.
func runTickCycle[T any](ctx context.Context, spanName, opAttr string, fn func(ctx context.Context) T) T {
	return observe.RunValue(ctx, cycleOp(spanName, opAttr), fn)
}

// cycleOp describes the span both envelopes open. spanName is the
// human-readable name matching the worker entry point; opAttr is the
// snake_case operation tag that lands on metrics and audit logs.
func cycleOp(spanName, opAttr string) observe.Op {
	return observe.Internal(spanName,
		[]attribute.KeyValue{telemetry.AttrOperation.String(opAttr)},
		nil)
}

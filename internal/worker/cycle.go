// -------------------------------------------------------------------------------
// Worker Cycle Envelope - Shared audit + tracing scaffolding
//
// Author: Alex Freidah
//
// runOpsCycle wraps the audit.WithRequestID + observe.Run + observe.Internal
// envelope every operator-driven worker cycle uses. Use it for the public
// entry points of workers that perform a discrete unit of work and return a
// count + error (Replicate, Rebalance, OverReplicationClean). Periodic
// background scans that don't return counts use telemetry.StartSpan directly.
// -------------------------------------------------------------------------------

package worker

import (
	"context"

	"go.opentelemetry.io/otel/attribute"

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// runOpsCycle attaches a fresh request ID, opens an internal-operation span
// named spanName, sets the operation attribute to opAttr, and invokes fn
// inside both. Returns whatever fn returns. spanName is the human-readable
// span name (PascalCase, matches the worker entry point); opAttr is the
// snake_case operation tag that lands on metrics and audit logs.
func runOpsCycle[T any](ctx context.Context, spanName, opAttr string, fn func(ctx context.Context) (T, error)) (T, error) {
	ctx = audit.WithRequestID(ctx, audit.NewID())
	return observe.Run(ctx,
		observe.Internal(spanName,
			[]attribute.KeyValue{telemetry.AttrOperation.String(opAttr)},
			nil),
		fn)
}

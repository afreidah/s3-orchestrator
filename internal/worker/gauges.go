// -------------------------------------------------------------------------------
// Worker Gauges - Sharing What the Lock Holder Computed
//
// Author: Alex Freidah
//
// A lock-gated worker computes fleet-wide gauges only on the instance holding
// its lock, so it publishes them rather than setting them locally; every
// instance then serves the same figures. A worker built without a publisher -
// a single instance, or a test - sets them locally instead.
// -------------------------------------------------------------------------------

package worker

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// GaugePublisher shares a worker's fleet-wide gauges with every instance.
// *metrics.Collector satisfies it.
type GaugePublisher interface {
	PublishWorkerGauges(ctx context.Context, source string, g telemetry.WorkerGauges)
}

// publishGauges publishes g through p, or applies it locally when no publisher
// is wired.
func publishGauges(ctx context.Context, p GaugePublisher, source string, g telemetry.WorkerGauges) {
	if p == nil {
		g.Apply()
		return
	}
	p.PublishWorkerGauges(ctx, source, g)
}

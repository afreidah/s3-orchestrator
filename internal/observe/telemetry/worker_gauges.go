// -------------------------------------------------------------------------------
// Worker Gauges - Fleet-Wide Figures a Lock-Holding Worker Computes
//
// Author: Alex Freidah
//
// Some gauges describe a pass in progress, such as how many moves a rebalance
// has planned, which only the instance holding the worker's advisory lock
// knows. WorkerGauges carries one worker's figures so that instance can
// publish them and every instance serve the same values. A figure the store
// can count is read by every instance itself, with the fleet figures.
// -------------------------------------------------------------------------------

package telemetry

import "context"

// GaugeSourceRebalancer is the rebalancer's shared-state key. Each key has a
// single writer at a time: the instance holding that worker's lock.
const GaugeSourceRebalancer = "rebalancer"

// GaugeSources lists every worker that publishes gauges, for the loader that
// reads them all back.
var GaugeSources = []string{GaugeSourceRebalancer}

// WorkerGauges is one worker's published figures. A nil field is one that
// worker does not own, and applying the value leaves that gauge untouched.
type WorkerGauges struct {
	RebalancePending *int64 `json:"rebalance_pending,omitempty"`
}

// Apply sets every gauge the value carries.
func (g *WorkerGauges) Apply() {
	setIf(RebalancePending.Set, g.RebalancePending)
}

// GaugePublisher shares a worker's fleet-wide gauges with every instance.
// *metrics.Collector satisfies it, applying them locally as well.
type GaugePublisher interface {
	PublishWorkerGauges(ctx context.Context, source string, g WorkerGauges)
}

// setIf sets a gauge from a value the published figures carry, leaving it
// alone when they have none.
func setIf(set func(float64), v *int64) {
	if v != nil {
		set(float64(*v))
	}
}

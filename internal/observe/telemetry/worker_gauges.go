// -------------------------------------------------------------------------------
// Worker Gauges - Fleet-Wide Figures a Lock-Holding Worker Computes
//
// Author: Alex Freidah
//
// Some gauges describe state every instance shares - a queue's depth, how far
// verification is behind - but only the instance holding the worker's advisory
// lock on a tick computes them. WorkerGauges carries one worker's figures so
// that instance can publish them and every instance serve the same values,
// instead of each one exporting whatever it last computed before the lock
// moved elsewhere.
// -------------------------------------------------------------------------------

package telemetry

// The workers that publish gauges, one shared-state key each. Each key has a
// single writer at a time: the instance holding that worker's lock.
const (
	GaugeSourceCleanup       = "cleanup"
	GaugeSourcePendingReaper = "pending_reaper"
	GaugeSourceScrubber      = "scrubber"
	GaugeSourceRebalancer    = "rebalancer"
	GaugeSourceNotifier      = "notifier"
)

// GaugeSources lists every worker that publishes gauges, for the loader that
// reads them all back.
var GaugeSources = []string{
	GaugeSourceCleanup,
	GaugeSourcePendingReaper,
	GaugeSourceScrubber,
	GaugeSourceRebalancer,
	GaugeSourceNotifier,
}

// WorkerGauges is one worker's published figures. A nil field is one that
// worker does not own, and applying the value leaves that gauge untouched.
type WorkerGauges struct {
	CleanupQueueDepth       *int64   `json:"cleanup_queue_depth,omitempty"`
	CleanupDLQDepth         *int64   `json:"cleanup_dlq_depth,omitempty"`
	PendingIntentsDepth     *int64   `json:"pending_intents_depth,omitempty"`
	OldestUnverifiedSeconds *float64 `json:"oldest_unverified_seconds,omitempty"`
	NeverVerifiedCopies     *int64   `json:"never_verified_copies,omitempty"`
	DeferredCopies          *int64   `json:"deferred_copies,omitempty"`
	RebalancePending        *int64   `json:"rebalance_pending,omitempty"`
	NotificationQueueDepth  *int64   `json:"notification_queue_depth,omitempty"`
}

// Apply sets every gauge the value carries.
func (g *WorkerGauges) Apply() {
	setIf(CleanupQueueDepth.Set, g.CleanupQueueDepth)
	setIf(CleanupDLQDepth.Set, g.CleanupDLQDepth)
	setIf(PendingIntentsDepth.Set, g.PendingIntentsDepth)
	if g.OldestUnverifiedSeconds != nil {
		IntegrityOldestUnverifiedSeconds.Set(*g.OldestUnverifiedSeconds)
	}
	setIf(IntegrityNeverVerifiedCopies.Set, g.NeverVerifiedCopies)
	setIf(IntegrityDeferredCopies.Set, g.DeferredCopies)
	setIf(RebalancePending.Set, g.RebalancePending)
	setIf(NotificationQueueDepth.Set, g.NotificationQueueDepth)
}

// setIf sets a gauge from a count the value carries.
func setIf(set func(float64), v *int64) {
	if v != nil {
		set(float64(*v))
	}
}

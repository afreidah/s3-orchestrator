// -------------------------------------------------------------------------------
// Metrics  -  Rebalancer
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface. Split out of the
// original 784-line metrics.go to keep each subsystem under ~150 lines.
// -------------------------------------------------------------------------------

package telemetry

// Rebalancer progress metrics.
var (
	// RebalanceObjectsMoved counts objects moved by the rebalancer.
	RebalanceObjectsMoved = counterVec("s3o_rebalance_objects_moved_total",
		"Total number of objects moved by the rebalancer", "strategy", "status")

	// RebalanceBytesMoved counts bytes moved by the rebalancer.
	RebalanceBytesMoved = counterVec("s3o_rebalance_bytes_moved_total",
		"Total bytes moved by the rebalancer", "strategy")

	// RebalanceSkipped counts rebalancer runs that were skipped.
	RebalanceSkipped = counterVec("s3o_rebalance_skipped_total",
		"Total number of rebalancer runs skipped", "reason")

	// RebalancePending tracks objects planned for rebalance in the current cycle.
	RebalancePending = gauge("s3o_rebalance_pending", "Number of objects planned for rebalance")
)

// -------------------------------------------------------------------------------
// Metrics  -  Rebalancer
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface. Split out of the
// original 784-line metrics.go to keep each subsystem under ~150 lines.
// -------------------------------------------------------------------------------

package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Rebalancer progress metrics.
var (
	// RebalanceObjectsMoved counts objects moved by the rebalancer.
	RebalanceObjectsMoved = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_rebalance_objects_moved_total",
			Help: "Total number of objects moved by the rebalancer",
		},
		[]string{"strategy", "status"},
	)

	// RebalanceBytesMoved counts bytes moved by the rebalancer.
	RebalanceBytesMoved = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_rebalance_bytes_moved_total",
			Help: "Total bytes moved by the rebalancer",
		},
		[]string{"strategy"},
	)

	// RebalanceSkipped counts rebalancer runs that were skipped.
	RebalanceSkipped = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_rebalance_skipped_total",
			Help: "Total number of rebalancer runs skipped",
		},
		[]string{"reason"},
	)

	// RebalancePending tracks objects planned for rebalance in the current cycle.
	RebalancePending = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_rebalance_pending",
			Help: "Number of objects planned for rebalance",
		},
	)
)

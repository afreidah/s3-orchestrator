// -------------------------------------------------------------------------------
// Metrics  -  Replication, Over-Replication
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

// ReplicationPending and related package-level variables used by this package.
var (
	// --- Replication metrics ---

	// ReplicationPending tracks objects currently below the target replication factor.
	ReplicationPending = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_replication_pending",
			Help: "Number of objects below the target replication factor",
		},
	)

	// ReplicationCopiesCreatedTotal counts replica copies created.
	ReplicationCopiesCreatedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_replication_copies_created_total",
			Help: "Total number of replica copies created",
		},
	)

	// ReplicationErrorsTotal counts replication errors.
	ReplicationErrorsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_replication_errors_total",
			Help: "Total number of replication errors",
		},
	)

	// ReplicationDuration tracks replication worker cycle time.
	ReplicationDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "s3o_replication_duration_seconds",
			Help:    "Replication worker cycle time in seconds",
			Buckets: []float64{1, 5, 10, 30, 60, 120, 300, 600},
		},
	)

	// ReplicationRunsTotal counts replication worker executions.
	ReplicationRunsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_replication_runs_total",
			Help: "Total number of replication worker runs",
		},
		[]string{"status"},
	)

	// ReplicationHealthCopiesTotal counts copies created to replace copies on
	// circuit-broken backends.
	ReplicationHealthCopiesTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_replication_health_copies_total",
			Help: "Replica copies created to replace copies on circuit-broken backends",
		},
	)

	// --- Over-replication cleanup metrics ---

	// OverReplicationPending tracks objects currently above the target replication factor.
	OverReplicationPending = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_over_replication_pending",
			Help: "Number of objects above the target replication factor",
		},
	)

	// OverReplicationRemovedTotal counts excess copies removed by the cleaner.
	OverReplicationRemovedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_over_replication_removed_total",
			Help: "Total excess copies removed by over-replication cleanup",
		},
	)

	// OverReplicationErrorsTotal counts over-replication cleanup errors.
	OverReplicationErrorsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_over_replication_errors_total",
			Help: "Total number of over-replication cleanup errors",
		},
	)

	// OverReplicationRunsTotal counts over-replication cleanup worker executions.
	OverReplicationRunsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_over_replication_runs_total",
			Help: "Total number of over-replication cleanup worker runs",
		},
		[]string{"status"},
	)

	// OverReplicationDuration tracks over-replication cleanup worker cycle time.
	OverReplicationDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "s3o_over_replication_duration_seconds",
			Help:    "Over-replication cleanup worker cycle time in seconds",
			Buckets: []float64{1, 5, 10, 30, 60, 120, 300, 600},
		},
	)

)

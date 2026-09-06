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

// Replication backlog and copy-creation metrics.
var (
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

	// ReplicationWriteCopiesTotal counts the further copies a write placed
	// itself rather than leaving to the replicator, by what became of each.
	// Outcome is one of: committed, untrusted (a newer write took the key while
	// the upload ran), failed (the backend refused it).
	//
	// A rising untrusted count is overwrites racing their own extra copies, and
	// each one costs a rebuild; a rising failed count is the fan-out asking more
	// of the backends than they will take, and both mean the replicator is
	// picking up work this was meant to save.
	ReplicationWriteCopiesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_replication_write_copies_total",
			Help: "Further copies placed during the write, by outcome",
		},
		[]string{"outcome"},
	)

	// DetachedUploadsDepth tracks the writes whose copies are still uploading
	// after their client was answered.
	//
	// The number a healthy fleet sits at is roughly the write rate times how
	// long a copy takes, so a handful. A rising depth is the signal a backend
	// is slow rather than broken: nothing fails, nothing retries, and the work
	// simply accumulates until the ceiling turns the fan-out off.
	DetachedUploadsDepth = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_detached_uploads_depth",
			Help: "Writes whose further copies are still uploading after the response",
		},
	)

	// ReplicationWriteFanoutSkippedTotal counts writes that placed a single
	// copy because no slot was free, leaving the rest to the replicator.
	//
	// Every one of these costs what the fan-out exists to avoid - a read of the
	// object plus the source backend's egress - so a sustained rate means
	// either the ceiling is too low for the write rate or a backend is falling
	// behind. The depth gauge above says which.
	ReplicationWriteFanoutSkippedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_replication_write_fanout_skipped_total",
			Help: "Writes that fell back to a single copy because no detached-upload slot was free",
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

	// OverReplicationKeyPreservedTotal counts copies the over-replication
	// cleaner declined to remove because they held the only usable encryption
	// key for their object. A non-zero value means some copy set disagrees
	// about encryption and wants repair.
	OverReplicationKeyPreservedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_over_replication_key_preserved_total",
			Help: "Copies kept because they held the only usable encryption key for the object",
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

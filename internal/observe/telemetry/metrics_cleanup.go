// -------------------------------------------------------------------------------
// Metrics  -  Cleanup Queue, Lifecycle, Drain
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface.
// -------------------------------------------------------------------------------

package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Cleanup retry queue metrics.
var (
	// CleanupQueueEnqueuedTotal counts the rows the request path and the
	// workers settle as their transactions queue them, by reason. Every copy,
	// intent or part a transaction stops recording is queued, so this is the
	// rate bytes are given up for deletion, not the rate deletes fail.
	CleanupQueueEnqueuedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_cleanup_queue_enqueued_total",
			Help: "Total rows queued for deletion by the transactions that stopped recording their bytes",
		},
		[]string{"reason"},
	)

	// CleanupQueueProcessedTotal counts cleanup rows settled, whether on the
	// request path right after their transaction or by the cleanup worker.
	CleanupQueueProcessedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_cleanup_queue_processed_total",
			Help: "Total cleanup queue rows settled, by outcome",
		},
		[]string{"status"},
	)

	// CleanupQueueDepth tracks the current number of pending cleanup items.
	CleanupQueueDepth = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_cleanup_queue_depth",
			Help: "Current number of pending items in the cleanup retry queue",
		},
	)

	// CleanupDLQDepth tracks the current number of rows in the cleanup
	// dead-letter table - cleanup_queue rows that exhausted their retry
	// budget without ever succeeding at the physical backend delete. A
	// non-zero value means orphan bytes are still on the backend with
	// no automatic recovery in flight; operators must investigate.
	CleanupDLQDepth = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_cleanup_dlq_depth",
			Help: "Current number of unrecoverable orphans in the cleanup dead-letter queue",
		},
	)

	// CleanupDLQEnqueuedTotal counts cleanup_queue rows graduated to the
	// dead-letter table per backend, labelled so dashboards can pinpoint
	// which backend is failing physical deletes.
	CleanupDLQEnqueuedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_cleanup_dlq_enqueued_total",
			Help: "Total cleanup_queue rows moved to cleanup_dlq after exhausting retries",
		},
		[]string{"backend"},
	)

	// CleanupQueueStaleClaimsRecoveredTotal counts cleanup_queue rows whose
	// claim was reclaimed because the previous holder did not finalise the
	// row within the configured grace period. A non-zero rate is operational
	// signal that a worker died mid-process or the grace period is too
	// short for the realistic worst-case processing time.
	CleanupQueueStaleClaimsRecoveredTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_cleanup_queue_stale_claims_recovered_total",
			Help: "cleanup_queue rows whose stale claim was reclaimed by a later worker tick",
		},
		[]string{"backend"},
	)

	// PendingIntentsEnqueuedTotal counts pending intents inserted by the
	// write path before the backend PUT.
	PendingIntentsEnqueuedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_pending_intents_enqueued_total",
			Help: "Total in-flight PUT intents inserted before the backend write",
		},
	)

	// PendingIntentsResolvedTotal counts intents resolved by the reaper or
	// the synchronous commit path. Status is one of: committed, promoted,
	// dropped, ambiguous, already_resolved.
	PendingIntentsResolvedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_pending_intents_resolved_total",
			Help: "Total pending PUT intents resolved by status",
		},
		[]string{"status"},
	)

	// PendingIntentsDepth tracks the current number of unresolved pending
	// intents in the database.
	PendingIntentsDepth = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_pending_intents_depth",
			Help: "Current number of unresolved pending PUT intents",
		},
	)

	// LifecycleDeletedTotal counts objects deleted by lifecycle expiration rules.
	LifecycleDeletedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_lifecycle_deleted_total",
			Help: "Objects deleted by lifecycle expiration rules",
		},
	)

	// LifecycleFailedTotal counts objects that failed lifecycle deletion.
	LifecycleFailedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_lifecycle_failed_total",
			Help: "Objects that failed lifecycle deletion",
		},
	)

	// LifecycleRunsTotal counts lifecycle worker executions.
	LifecycleRunsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_lifecycle_runs_total",
			Help: "Lifecycle worker executions",
		},
		[]string{"status"},
	)

	// DrainObjectsMoved counts objects moved during backend drain operations.
	DrainObjectsMoved = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_drain_objects_moved_total",
			Help: "Total number of objects moved during backend drain operations",
		},
	)

	// DrainBytesMoved counts bytes moved during backend drain operations.
	DrainBytesMoved = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_drain_bytes_moved_total",
			Help: "Total bytes moved during backend drain operations",
		},
	)

	// DrainActive is the number of backends whose drain record is in
	// progress, set from the records by the drainer on each tick.
	DrainActive = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_drain_active",
			Help: "Number of backends with a drain in progress",
		},
	)

	// WriteTargetRefusedTotal counts PutObject attempts that landed bytes on a
	// backend which stopped accepting writes mid-write, because it was
	// cordoned or a drain started. The orchestrator detects it after the
	// backend PUT completes, discards the intent and queues its bytes for
	// deletion, and fails the attempt over to the next eligible backend.
	WriteTargetRefusedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_write_target_refused_total",
			Help: "Number of PutObject attempts aborted because the backend stopped accepting writes mid-write",
		},
	)

	// BackendState is 1 for each state flag a backend carries, labeled by
	// backend and flag name, and absent for a backend in normal service. It is
	// republished from the records on every state cache refresh.
	BackendState = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_backend_state",
			Help: "1 for each state flag a backend carries (cordoned, draining, drained, drain-failed)",
		},
		[]string{"backend", "state"},
	)
)

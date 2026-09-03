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
	// CleanupQueueEnqueuedTotal counts items added to the cleanup retry queue.
	CleanupQueueEnqueuedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_cleanup_queue_enqueued_total",
			Help: "Total items added to the cleanup retry queue",
		},
		[]string{"reason"},
	)

	// CleanupEnqueueFailuresTotal counts cleanup-queue enqueue attempts
	// that failed after a backend write already succeeded. The orphan
	// object exists on the backend but the system lost the chance to
	// track it for retry. stage="enqueue" means the cleanup_queue row
	// itself did not persist (worst case  -  cleanup-queue worker will
	// never see this orphan); stage="orphan_bytes" means the row
	// persisted but the orphan_bytes counter did not increment (quota
	// accounting drifts but cleanup still runs). Operators alert on any
	// non-zero rate of stage="enqueue" and run the reconciler
	// (POST /admin/api/reconcile) once DB connectivity returns.
	CleanupEnqueueFailuresTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_cleanup_enqueue_failures_total",
			Help: "Cleanup-queue enqueue attempts that failed after a successful backend write",
		},
		[]string{"backend", "reason", "stage"},
	)

	// CleanupQueueProcessedTotal counts items processed from the cleanup queue.
	CleanupQueueProcessedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_cleanup_queue_processed_total",
			Help: "Total items processed from the cleanup retry queue",
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

	// DrainActive is the live count of in-flight drain operations.
	// Inc'd on StartDrain and Dec'd on completion (success, cancel, or
	// abort) so concurrent drains across different backends do not
	// clobber each other's state the way a Set(0)/Set(1) gauge would.
	// 0 means no drains are running.
	DrainActive = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_drain_active",
			Help: "Count of in-flight backend drain operations (Inc/Dec so concurrent drains compose)",
		},
	)

	// DrainRaceAbortedTotal counts PutObject attempts that landed bytes
	// on a backend whose drain started mid-write. The orchestrator
	// detects the race after the backend PUT completes, deletes the
	// orphaned bytes, and fails the attempt over to the next eligible
	// backend; this counter pins how often the race fires in production
	// so the drain timing assumptions can be revisited if it climbs.
	DrainRaceAbortedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_drain_race_aborted_total",
			Help: "Number of PutObject attempts aborted after drain started mid-write",
		},
	)
)

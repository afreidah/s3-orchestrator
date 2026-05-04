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

// CleanupQueueEnqueuedTotal and related package-level variables used by this package.
var (
	// --- Cleanup queue metrics ---

	// CleanupQueueEnqueuedTotal counts items added to the cleanup retry queue.
	CleanupQueueEnqueuedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_cleanup_queue_enqueued_total",
			Help: "Total items added to the cleanup retry queue",
		},
		[]string{"reason"},
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

	// --- Pending objects (write-path PUT-before-COMMIT pattern) ---

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

	// --- Lifecycle metrics ---

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

	// --- Drain metrics ---

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

	// DrainActive is 1 when a drain operation is in progress, 0 otherwise.
	DrainActive = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_drain_active",
			Help: "Whether a backend drain operation is currently in progress",
		},
	)

)

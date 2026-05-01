// -------------------------------------------------------------------------------
// Metrics — Cleanup Queue, Lifecycle, Drain
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

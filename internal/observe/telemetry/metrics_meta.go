// -------------------------------------------------------------------------------
// Metrics — Build Info, Notifications
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

var (
	// --- Info metric ---

	// BuildInfo exposes version information.
	BuildInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_build_info",
			Help: "Build information for the S3 proxy",
		},
		[]string{"version", "go_version"},
	)

	// --- Notification metrics ---

	// NotificationSentTotal counts successfully delivered webhook notifications.
	NotificationSentTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_notification_sent_total",
			Help: "Webhook notifications delivered successfully",
		},
		[]string{"endpoint", "event_type"},
	)

	// NotificationFailedTotal counts webhook delivery failures.
	NotificationFailedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_notification_failed_total",
			Help: "Webhook notification delivery failures",
		},
		[]string{"endpoint", "event_type"},
	)

	// NotificationDroppedTotal counts events dropped due to dampening or enqueue failure.
	NotificationDroppedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_notification_dropped_total",
			Help: "Events dropped due to dampening or queue insertion failure",
		},
	)

	// NotificationStoreErrorsTotal counts outbox-store operation failures in
	// the delivery worker (CompleteNotification / RetryNotification). A
	// non-zero value means the worker saw a store error that could cause
	// duplicate or dropped webhook deliveries — alert on any increase.
	NotificationStoreErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_notification_store_errors_total",
			Help: "Outbox store errors seen by the notification delivery worker",
		},
		[]string{"operation"},
	)

	// NotificationQueueDepth reports the number of pending notifications in the outbox.
	NotificationQueueDepth = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_notification_queue_depth",
			Help: "Pending notifications in the delivery outbox",
		},
	)

	// NotificationDuration measures webhook delivery latency.
	NotificationDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3o_notification_duration_seconds",
			Help:    "Webhook notification delivery latency",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"endpoint"},
	)
)

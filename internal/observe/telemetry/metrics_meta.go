// -------------------------------------------------------------------------------
// Metrics  -  Build Info, Notifications
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface. Split out of the
// original 784-line metrics.go to keep each subsystem under ~150 lines.
// -------------------------------------------------------------------------------

package telemetry

import "github.com/prometheus/client_golang/prometheus"

// Build information and webhook notification metrics.
var (
	// BuildInfo exposes version information.
	BuildInfo = gaugeVec("s3o_build_info", "Build information for the S3 proxy", "version", "go_version")

	// NotificationSentTotal counts successfully delivered webhook notifications.
	NotificationSentTotal = counterVec("s3o_notification_sent_total",
		"Webhook notifications delivered successfully", "endpoint", "event_type")

	// NotificationFailedTotal counts webhook delivery failures.
	NotificationFailedTotal = counterVec("s3o_notification_failed_total",
		"Webhook notification delivery failures", "endpoint", "event_type")

	// NotificationDroppedTotal counts events dropped due to dampening or enqueue failure.
	NotificationDroppedTotal = counter("s3o_notification_dropped_total",
		"Events dropped due to dampening or queue insertion failure")

	// NotificationStoreErrorsTotal counts outbox-store operation failures in
	// the delivery worker (CompleteNotification / RetryNotification). A
	// non-zero value means the worker saw a store error that could cause
	// duplicate or dropped webhook deliveries  -  alert on any increase.
	NotificationStoreErrorsTotal = counterVec("s3o_notification_store_errors_total",
		"Outbox store errors seen by the notification delivery worker", "operation")

	// NotificationQueueDepth reports the number of pending notifications in the outbox.
	NotificationQueueDepth = gauge("s3o_notification_queue_depth", "Pending notifications in the delivery outbox")

	// NotificationDuration measures webhook delivery latency.
	NotificationDuration = histogramVec("s3o_notification_duration_seconds",
		"Webhook notification delivery latency", prometheus.DefBuckets, "endpoint")
)

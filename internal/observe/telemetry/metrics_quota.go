// -------------------------------------------------------------------------------
// Metrics  -  Quota, Object, Multipart, Usage
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface. Split out of the
// original 784-line metrics.go to keep each subsystem under ~150 lines.
// -------------------------------------------------------------------------------

package telemetry

// Per-backend quota usage metrics.
var (
	// QuotaBytesUsed tracks current bytes used per backend.
	QuotaBytesUsed = gaugeVec("s3o_quota_bytes_used", "Current bytes used on each backend", "backend")

	// QuotaBytesLimit tracks quota limit per backend.
	QuotaBytesLimit = gaugeVec("s3o_quota_bytes_limit", "Quota limit in bytes for each backend", "backend")

	// QuotaBytesAvailable tracks available bytes per backend.
	QuotaBytesAvailable = gaugeVec("s3o_quota_bytes_available",
		"Available bytes (limit - used - orphan) for each backend", "backend")

	// QuotaOrphanBytes tracks bytes pending physical deletion per backend.
	QuotaOrphanBytes = gaugeVec("s3o_quota_orphan_bytes",
		"Bytes pending physical deletion (logically freed but not yet removed from backend)", "backend")

	// UsageReconcileCorrectionsTotal counts per-backend byte-total corrections
	// applied by usage reconciliation. The counter moves inside the transaction
	// that writes the rows it summarizes, so this should never rise: any
	// increase means a mutation path is not charging what it stored.
	UsageReconcileCorrectionsTotal = counter("s3o_quota_reconcile_corrections_total",
		"Byte-total drift corrections applied by usage reconciliation; expected to stay at zero")

	// QuotaClaimsDeclinedTotal counts writes a backend refused for want of
	// room, judged by the insert that claims the space rather than by any
	// in-memory figure. A backend appearing here is full: the write either
	// moved to another candidate or, if none had room, failed with 507.
	QuotaClaimsDeclinedTotal = counterVec("s3o_quota_claims_declined_total",
		"Write claims a backend declined for insufficient room", "backend")

	// ObjectCount tracks the number of objects stored per backend.
	ObjectCount = gaugeVec("s3o_objects_count", "Number of objects stored on each backend", "backend")

	// ActiveMultipartUploads tracks in-progress multipart uploads per backend.
	ActiveMultipartUploads = gaugeVec("s3o_active_multipart_uploads",
		"Number of in-progress multipart uploads per backend", "backend")

	// UsageAPIRequests tracks each backend's API request count in its current
	// usage cycle.
	UsageAPIRequests = gaugeVec("s3o_usage_api_requests",
		"API request count per backend in its current usage cycle (from DB)", "backend")

	// UsageCycleResetTimestamp publishes when each backend's usage cycle next
	// resets, so a dashboard can show the time left in the cycle.
	UsageCycleResetTimestamp = gaugeVec("s3o_usage_cycle_reset_timestamp_seconds",
		"Unix time each backend's usage cycle next resets", "backend")

	// UsagePoolRequests tracks each backend request pool's count in the
	// backend's current cycle. Pools are additive, so these do not sum to
	// s3o_usage_api_requests.
	UsagePoolRequests = gaugeVec("s3o_usage_pool_requests",
		"Request count per backend request pool in the current usage cycle (from DB)", "backend", "pool")

	// UsagePoolLimit publishes each pool's configured ceiling, so a dashboard
	// can show headroom without the limit being hardcoded alongside it.
	UsagePoolLimit = gaugeVec("s3o_usage_pool_limit",
		"Configured monthly request ceiling per backend request pool (0 = unlimited)", "backend", "pool")

	// UsageEgressBytes tracks each backend's egress bytes in its current cycle.
	UsageEgressBytes = gaugeVec("s3o_usage_egress_bytes",
		"Egress bytes per backend in its current usage cycle (from DB)", "backend")

	// UsageIngressBytes tracks each backend's ingress bytes in its current cycle.
	UsageIngressBytes = gaugeVec("s3o_usage_ingress_bytes",
		"Ingress bytes per backend in its current usage cycle (from DB)", "backend")

	// UsageEgressLimitBytes publishes each backend's monthly egress limit, so
	// egress can be read as a share of it.
	UsageEgressLimitBytes = gaugeVec("s3o_usage_egress_limit_bytes",
		"Configured monthly egress byte limit per backend (0 = unlimited)", "backend")

	// UsageIngressLimitBytes publishes each backend's monthly ingress limit, so
	// ingress can be read as a share of it.
	UsageIngressLimitBytes = gaugeVec("s3o_usage_ingress_limit_bytes",
		"Configured monthly ingress byte limit per backend (0 = unlimited)", "backend")

	// UsageWorkloadAPIRequests splits s3o_usage_api_requests by the workload
	// that made the calls.
	UsageWorkloadAPIRequests = gaugeVec("s3o_usage_workload_api_requests",
		"API request count per backend and workload in the current usage cycle (from DB)", "backend", "workload")

	// UsageWorkloadBytes splits s3o_usage_egress_bytes and
	// s3o_usage_ingress_bytes by workload; direction is egress or ingress.
	UsageWorkloadBytes = gaugeVec("s3o_usage_workload_bytes",
		"Bytes per backend, workload and direction in the current usage cycle (from DB)",
		"backend", "workload", "direction")

	// UsageLimitRejectionsTotal counts operations rejected due to monthly usage limits.
	UsageLimitRejectionsTotal = counterVec("s3o_usage_limit_rejections_total",
		"Total operations rejected due to monthly usage limits", "operation", "limit_type")
)

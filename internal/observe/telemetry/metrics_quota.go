// -------------------------------------------------------------------------------
// Metrics  -  Quota, Object, Multipart, Usage
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

// Per-backend quota usage metrics.
var (
	// QuotaBytesUsed tracks current bytes used per backend.
	QuotaBytesUsed = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_quota_bytes_used",
			Help: "Current bytes used on each backend",
		},
		[]string{"backend"},
	)

	// QuotaBytesLimit tracks quota limit per backend.
	QuotaBytesLimit = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_quota_bytes_limit",
			Help: "Quota limit in bytes for each backend",
		},
		[]string{"backend"},
	)

	// QuotaBytesAvailable tracks available bytes per backend.
	QuotaBytesAvailable = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_quota_bytes_available",
			Help: "Available bytes (limit - used - orphan) for each backend",
		},
		[]string{"backend"},
	)

	// QuotaOrphanBytes tracks bytes pending physical deletion per backend.
	QuotaOrphanBytes = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_quota_orphan_bytes",
			Help: "Bytes pending physical deletion (logically freed but not yet removed from backend)",
		},
		[]string{"backend"},
	)

	// UsageReconcileCorrectionsTotal counts per-backend byte-total corrections
	// applied by usage reconciliation. The counter moves inside the transaction
	// that writes the rows it summarizes, so this should never rise: any
	// increase means a mutation path is not charging what it stored.
	UsageReconcileCorrectionsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_quota_reconcile_corrections_total",
			Help: "Byte-total drift corrections applied by usage reconciliation; expected to stay at zero",
		},
	)

	// QuotaClaimsDeclinedTotal counts writes a backend refused for want of
	// room, judged by the insert that claims the space rather than by any
	// in-memory figure. A backend appearing here is full: the write either
	// moved to another candidate or, if none had room, failed with 507.
	QuotaClaimsDeclinedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_quota_claims_declined_total",
			Help: "Write claims a backend declined for insufficient room",
		},
		[]string{"backend"},
	)

	// ObjectCount tracks the number of objects stored per backend.
	ObjectCount = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_objects_count",
			Help: "Number of objects stored on each backend",
		},
		[]string{"backend"},
	)

	// ActiveMultipartUploads tracks in-progress multipart uploads per backend.
	ActiveMultipartUploads = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_active_multipart_uploads",
			Help: "Number of in-progress multipart uploads per backend",
		},
		[]string{"backend"},
	)

	// UsageAPIRequests tracks each backend's API request count in its current
	// usage cycle.
	UsageAPIRequests = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_usage_api_requests",
			Help: "API request count per backend in its current usage cycle (from DB)",
		},
		[]string{"backend"},
	)

	// UsageCycleResetTimestamp publishes when each backend's usage cycle next
	// resets, so a dashboard can show the time left in the cycle.
	UsageCycleResetTimestamp = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_usage_cycle_reset_timestamp_seconds",
			Help: "Unix time each backend's usage cycle next resets",
		},
		[]string{"backend"},
	)

	// UsagePoolRequests tracks each backend request pool's count in the
	// backend's current cycle. Pools are additive, so these do not sum to
	// s3o_usage_api_requests.
	UsagePoolRequests = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_usage_pool_requests",
			Help: "Request count per backend request pool in the current usage cycle (from DB)",
		},
		[]string{"backend", "pool"},
	)

	// UsagePoolLimit publishes each pool's configured ceiling, so a dashboard
	// can show headroom without the limit being hardcoded alongside it.
	UsagePoolLimit = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_usage_pool_limit",
			Help: "Configured monthly request ceiling per backend request pool (0 = unlimited)",
		},
		[]string{"backend", "pool"},
	)

	// UsageEgressBytes tracks each backend's egress bytes in its current cycle.
	UsageEgressBytes = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_usage_egress_bytes",
			Help: "Egress bytes per backend in its current usage cycle (from DB)",
		},
		[]string{"backend"},
	)

	// UsageIngressBytes tracks each backend's ingress bytes in its current cycle.
	UsageIngressBytes = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_usage_ingress_bytes",
			Help: "Ingress bytes per backend in its current usage cycle (from DB)",
		},
		[]string{"backend"},
	)

	// UsageEgressLimitBytes publishes each backend's monthly egress limit, so
	// egress can be read as a share of it.
	UsageEgressLimitBytes = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_usage_egress_limit_bytes",
			Help: "Configured monthly egress byte limit per backend (0 = unlimited)",
		},
		[]string{"backend"},
	)

	// UsageIngressLimitBytes publishes each backend's monthly ingress limit, so
	// ingress can be read as a share of it.
	UsageIngressLimitBytes = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_usage_ingress_limit_bytes",
			Help: "Configured monthly ingress byte limit per backend (0 = unlimited)",
		},
		[]string{"backend"},
	)

	// UsageWorkloadAPIRequests splits s3o_usage_api_requests by the workload
	// that made the calls.
	UsageWorkloadAPIRequests = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_usage_workload_api_requests",
			Help: "API request count per backend and workload in the current usage cycle (from DB)",
		},
		[]string{"backend", "workload"},
	)

	// UsageWorkloadBytes splits s3o_usage_egress_bytes and
	// s3o_usage_ingress_bytes by workload; direction is egress or ingress.
	UsageWorkloadBytes = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_usage_workload_bytes",
			Help: "Bytes per backend, workload and direction in the current usage cycle (from DB)",
		},
		[]string{"backend", "workload", "direction"},
	)

	// UsageLimitRejectionsTotal counts operations rejected due to monthly usage limits.
	UsageLimitRejectionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_usage_limit_rejections_total",
			Help: "Total operations rejected due to monthly usage limits",
		},
		[]string{"operation", "limit_type"},
	)
)

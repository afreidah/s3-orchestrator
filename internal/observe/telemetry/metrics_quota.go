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

// QuotaBytesUsed and related package-level variables used by this package.
var (
	// --- Quota metrics ---

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

	// --- Object metrics ---

	// ObjectCount tracks the number of objects stored per backend.
	ObjectCount = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_objects_count",
			Help: "Number of objects stored on each backend",
		},
		[]string{"backend"},
	)

	// --- Multipart metrics ---

	// ActiveMultipartUploads tracks in-progress multipart uploads per backend.
	ActiveMultipartUploads = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_active_multipart_uploads",
			Help: "Number of in-progress multipart uploads per backend",
		},
		[]string{"backend"},
	)

	// --- Usage tracking metrics ---

	// UsageAPIRequests tracks the current month's API request count per backend.
	UsageAPIRequests = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_usage_api_requests",
			Help: "Current month API request count per backend (from DB)",
		},
		[]string{"backend"},
	)

	// UsageEgressBytes tracks the current month's egress bytes per backend.
	UsageEgressBytes = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_usage_egress_bytes",
			Help: "Current month egress bytes per backend (from DB)",
		},
		[]string{"backend"},
	)

	// UsageIngressBytes tracks the current month's ingress bytes per backend.
	UsageIngressBytes = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3o_usage_ingress_bytes",
			Help: "Current month ingress bytes per backend (from DB)",
		},
		[]string{"backend"},
	)

	// --- Usage limit metrics ---

	// UsageLimitRejectionsTotal counts operations rejected due to monthly usage limits.
	UsageLimitRejectionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_usage_limit_rejections_total",
			Help: "Total operations rejected due to monthly usage limits",
		},
		[]string{"operation", "limit_type"},
	)

)

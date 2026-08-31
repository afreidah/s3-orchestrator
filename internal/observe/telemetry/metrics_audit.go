// -------------------------------------------------------------------------------
// Metrics  -  Audit
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

// AuditEventsTotal and related package-level variables used by this package.
var (

	// AuditEventsTotal counts audit log entries by event type.
	AuditEventsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_audit_events_total",
			Help: "Total number of audit log entries emitted",
		},
		[]string{"event"},
	)
)

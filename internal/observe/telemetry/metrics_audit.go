// -------------------------------------------------------------------------------
// Metrics  -  Audit
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface. Split out of the
// original 784-line metrics.go to keep each subsystem under ~150 lines.
// -------------------------------------------------------------------------------

package telemetry

// AuditEventsTotal counts audit log entries by event type.
var AuditEventsTotal = counterVec("s3o_audit_events_total", "Total number of audit log entries emitted", "event")

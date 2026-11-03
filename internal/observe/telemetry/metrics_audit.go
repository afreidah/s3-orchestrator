// -------------------------------------------------------------------------------
// Metrics  -  Audit
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface.
// -------------------------------------------------------------------------------

package telemetry

// AuditEventsTotal counts audit log entries by event type.
var AuditEventsTotal = counterVec("s3o_audit_events_total", "Total number of audit log entries emitted", "event")

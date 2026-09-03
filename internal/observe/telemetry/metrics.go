// -------------------------------------------------------------------------------
// Metrics - Prometheus Instrumentation Index
//
// Author: Alex Freidah
//
// Subsystem-scoped Prometheus surface for the S3 orchestrator. The metric
// definitions are split across metrics_<domain>.go files so each file stays
// under ~150 lines and one subsystem's metrics live alongside each other.
// All metrics are prefixed with "s3o_" and registered via promauto on init.
// -------------------------------------------------------------------------------

package telemetry

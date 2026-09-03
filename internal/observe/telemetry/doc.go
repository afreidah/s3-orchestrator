// Package telemetry provides the Prometheus metric surface and the
// OpenTelemetry tracing setup for the orchestrator.
//
// Metric definitions are split across metrics_<domain>.go files so one
// subsystem's metrics live together; all are prefixed "s3o_" and registered via
// promauto on init. The package also holds the in-memory log ring buffer the
// operator dashboard reads, fed by a handler that tees slog records to both
// stdout and the buffer.
package telemetry

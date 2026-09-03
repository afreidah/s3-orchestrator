// Package tickrunner provides the shared lifecycle.Runner that drives a worker
// function on a fixed interval under a PostgreSQL advisory lock.
//
// It owns audit-context creation per tick, lock-busy and startup-jitter
// handling, per-service health snapshotting, and the worker-tick telemetry
// counters. It lives outside internal/di so worker subsystems can construct
// their own services without going through DI, leaving DI focused on wiring.
package tickrunner

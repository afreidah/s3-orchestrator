// Package readpath orchestrates the read protocol on behalf of the object
// manager: resolve every location row for a key and try each backend in stored
// order, or, when the database is unavailable, broadcast over the whole fleet
// and remember the winner for the next degraded read.
//
// Callers supply a probe that knows how to perform the backend operation and
// how to release its per-attempt timeout. Failover does not care what the
// operation is, only that the probe reports a size and a cleanup function, so
// all the per-operation telemetry lives here rather than at every call site.
package readpath

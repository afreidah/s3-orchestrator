// Package drain owns the backend drain/remove lifecycle. It tracks the
// draining state map, runs the migration goroutine, and exposes IsDraining
// for the proxy core's eligibility filters.
package drain

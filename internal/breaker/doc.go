// Package breaker implements a generic three-state circuit breaker (closed,
// open, half-open) with pluggable error filters and probe jitter. The
// breaker emits no metrics or events on its own  -  callers wire those via
// the optional OnStateChange callback so this package stays free of
// observability dependencies.
package breaker

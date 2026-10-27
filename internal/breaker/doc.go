// Package breaker implements a generic two-state circuit breaker (closed,
// open) with pluggable error filters, and the RecoveryProber that closes an
// open breaker once a check of its dependency passes. The breaker emits no
// metrics or events on its own  -  callers wire those via the optional
// OnStateChange callback so this package stays free of observability
// dependencies.
package breaker

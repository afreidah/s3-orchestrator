// -------------------------------------------------------------------------------
// Accounting Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow interfaces the Recorder needs from the underlying usage
// tracker and metrics collector. Declared here so the accounting
// package does not import internal/proxy/infra (which would create
// a cycle) and so tests can mock at this granularity.
//
// Mirrors the consumer-declared-interfaces pattern documented in
// docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package accounting

// UsageTracker is the subset of *counter.UsageTracker the Recorder
// uses to credit per-backend API call and ingress/egress byte counters.
type UsageTracker interface {
	Record(backendName string, apiCalls, egressBytes, ingressBytes int64)
}

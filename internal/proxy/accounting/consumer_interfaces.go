// -------------------------------------------------------------------------------
// Accounting Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow contracts the Recorder pulls from the underlying usage
// tracker and metrics collector. Declared here so accounting does not
// import internal/proxy/infra (cycle break). Pattern rationale:
// docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package accounting

// UsageTracker is the subset of *counter.UsageTracker the Recorder
// uses to credit per-backend API call and ingress/egress byte counters.
type UsageTracker interface {
	Record(backendName string, apiCalls, egressBytes, ingressBytes int64)
}

// -------------------------------------------------------------------------------
// Admin CLI - usage commands (usage-flush, usage-reconcile)
//
// Author: Alex Freidah
//
// Operates on the per-backend usage accounting: usage-flush forces the in-memory
// or Redis counters out to backend_usage so dashboards catch up without waiting
// for the next tick; usage-reconcile recomputes bytes_used from the object
// ledger to correct drift in the incrementally maintained quota counter.
// -------------------------------------------------------------------------------

package adminctl

// cmdUsageFlush implements `s3-orchestrator admin usage-flush`. Triggers an
// out-of-band flush of the usage counters to backend_usage.
func cmdUsageFlush(_ []string, c *client) int {
	return c.post("/admin/api/usage-flush", "", nil)
}

// cmdUsageReconcile implements `s3-orchestrator admin usage-reconcile`.
// Recomputes each backend's bytes_used from the object ledger and prints the
// per-backend corrections applied.
func cmdUsageReconcile(_ []string, c *client) int {
	return c.post("/admin/api/usage-reconcile", "", nil)
}

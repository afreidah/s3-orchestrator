// -------------------------------------------------------------------------------
// Admin CLI - rebalance
//
// Author: Alex Freidah
//
// Triggers a one-shot rebalance pass on demand instead of waiting for the
// scheduled worker, so an operator can converge object distribution right
// after adding or resizing a backend. Runs the configured strategy, or
// spread-with-defaults when rebalance was never configured.
// -------------------------------------------------------------------------------

package adminctl

// cmdRebalance implements `s3-orchestrator admin rebalance`. Reports the
// number of objects moved.
func cmdRebalance(_ []string, c *client) int {
	return c.post("/admin/api/rebalance", "", nil)
}

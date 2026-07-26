// -------------------------------------------------------------------------------
// Admin API - Shared Rebalance DTOs
//
// Author: Alex Freidah
//
// Wire type for the on-demand rebalance endpoint shared by the handler and its
// clients. Kept in the leaf adminapi package so the server and its clients
// depend on one definition and the JSON shape cannot drift.
// -------------------------------------------------------------------------------

package adminapi

// RebalanceResponse is the outcome of one rebalance cycle. Status is "ok" when
// the pass ran and "skipped" when the rebalancer is not wired; Reason carries
// the explanation on the skipped path and is absent otherwise, so both
// outcomes share one shape.
type RebalanceResponse struct {
	Status string `json:"status"`
	Moved  int    `json:"moved"`
	Reason string `json:"reason,omitempty"`
}

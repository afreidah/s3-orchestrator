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

// RebalanceResponse is the outcome of one rebalance cycle. A cycle is skipped
// when the rebalancer is not wired.
type RebalanceResponse struct {
	Outcome
	Moved int `json:"moved"`
}

// -------------------------------------------------------------------------------
// Admin API - Lifecycle DTO
//
// Author: Alex Freidah
//
// Wire type for the on-demand expiration sweep, shared by the handler and its
// clients. Kept in the leaf adminapi package so the server and the
// out-of-process client depend on one definition.
// -------------------------------------------------------------------------------

package adminapi

// LifecycleResponse is the outcome of one expiration sweep. A sweep is skipped
// when there is nothing to run - no rules configured, or no manager wired.
// Failed separates a sweep whose deletes all failed from one that found
// nothing expired.
type LifecycleResponse struct {
	Outcome
	Deleted int `json:"deleted"`
	Failed  int `json:"failed"`
}

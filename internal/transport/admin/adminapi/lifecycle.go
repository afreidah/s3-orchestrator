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

// LifecycleResponse is the outcome of one expiration sweep. Status is "ok" when
// the sweep ran and "skipped" when there was nothing to run - no rules
// configured, or no manager wired - with Reason carrying which, so both
// outcomes share one shape.
//
// Deleted and Failed are reported separately because a sweep that removed
// nothing because every delete failed is a different answer from one that found
// nothing expired, and the whole point of the endpoint is telling those apart.
type LifecycleResponse struct {
	Status  string `json:"status"`
	Deleted int    `json:"deleted"`
	Failed  int    `json:"failed"`
	Reason  string `json:"reason,omitempty"`
}

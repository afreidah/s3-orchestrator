// -------------------------------------------------------------------------------
// Admin API - Pass Outcome
//
// Author: Alex Freidah
//
// The status and skip reason every on-demand pass reports the same way. Each
// pass response embeds it, so the JSON stays flat and every endpoint speaks
// one vocabulary for whether the pass ran.
// -------------------------------------------------------------------------------

package adminapi

// Outcome is the part every pass reports the same way. Status is "ok" when the
// pass ran and "skipped" when it had nothing to run against, such as a feature
// that is not configured; Reason accompanies "skipped" only.
type Outcome struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

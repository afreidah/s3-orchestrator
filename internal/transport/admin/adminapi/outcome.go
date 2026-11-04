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

// Outcome is the part every pass reports the same way, in the words a stream's
// result reports: Status is adminstream.OutcomeOK when the pass ran and
// adminstream.OutcomeSkipped when it had nothing to run against, such as a
// feature that is not configured; Reason accompanies a skip only.
type Outcome struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

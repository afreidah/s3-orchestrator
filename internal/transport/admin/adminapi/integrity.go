// -------------------------------------------------------------------------------
// Admin API - Shared Integrity DTOs
//
// Author: Alex Freidah
//
// Wire types for the integrity endpoints (scrub, checksum backfill, reconcile)
// shared by the handler and its clients. Kept in the leaf adminapi package so
// the server and its out-of-process client depend on one definition and the
// JSON shape cannot drift.
// -------------------------------------------------------------------------------

package adminapi

// IntegrityOutcome is the part every integrity pass reports the same way: the
// terminal status, and why the pass did nothing when it was skipped. Status is
// "ok" when the pass ran and "skipped" when integrity verification is
// disabled; Reason accompanies "skipped" only.
type IntegrityOutcome struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// ScrubResponse reports a scrub pass: how many stored objects had their
// content hash verified against backend data, and how many did not match.
type ScrubResponse struct {
	IntegrityOutcome
	Checked int `json:"checked"`
	Failed  int `json:"failed"`
}

// BackfillChecksumsResponse reports a checksum backfill pass. Done is true
// when no unhashed objects remained after the pass, so a caller draining the
// backlog in batches knows when to stop.
type BackfillChecksumsResponse struct {
	IntegrityOutcome
	Processed int  `json:"processed"`
	Done      bool `json:"done"`
}

// ReconcileResponse reports a reconcile pass: objects adopted from backend
// storage into the ledger, ledger rows dropped for objects no longer present,
// and how many backends were walked.
type ReconcileResponse struct {
	IntegrityOutcome
	Imported        int `json:"imported"`
	Removed         int `json:"removed"`
	BackendsScanned int `json:"backends_scanned"`
}

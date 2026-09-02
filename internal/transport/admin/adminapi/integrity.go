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

// ScrubKeyResponse reports an on-demand verification of one key, one entry per
// copy.
//
// Per-copy rather than a summary because that is where the useful asymmetry
// lives: a replicated object can have one copy intact and another corrupt, and
// a single verdict for the key would hide which backend is at fault.
type ScrubKeyResponse struct {
	Key    string            `json:"key"`
	Copies []CopyScrubResult `json:"copies"`
}

// CopyScrubResult is one copy's verdict.
type CopyScrubResult struct {
	Backend string `json:"backend"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// The values CopyScrubResult.Outcome takes. NotHashed means there was no stored
// hash to compare against, which is not the same as passing. Defined here rather
// than in the worker so every client reads the vocabulary from the wire package
// instead of importing the server's internals to interpret a response.
const (
	CopyVerified   = "verified"
	CopyMismatch   = "mismatch"
	CopyUnreadable = "unreadable"
	CopyNotHashed  = "not_hashed"
)

// ScrubResponse reports a scrub pass: how many stored copies had their content
// hash verified against backend data, how many did not match, and how many
// could not be read at all.
//
// Unreadable is reported separately because it is not a hash result. A pass
// that could not read a single copy has nothing to say about whether the
// bytes are intact, and reporting only Checked and Failed makes that pass look
// like a clean one. Deferred is distinct again: those copies were never
// attempted, because the backend holding them is over its usage limit.
type ScrubResponse struct {
	IntegrityOutcome
	Checked    int `json:"checked"`
	Failed     int `json:"failed"`
	Unreadable int `json:"unreadable"`
	Deferred   int `json:"deferred"`
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

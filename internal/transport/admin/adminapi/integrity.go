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

import "time"

// ScrubKeyResponse reports an on-demand verification of one key, one entry per
// copy, so a corrupt copy is attributed to its backend.
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
// could not be read at all. Unreadable copies say nothing about integrity, so
// they are not counted as checked. Deferred copies were not attempted because
// their backend is over its usage limit.
type ScrubResponse struct {
	Outcome
	Checked    int `json:"checked"`
	Failed     int `json:"failed"`
	Unreadable int `json:"unreadable"`
	Deferred   int `json:"deferred"`
}

// BackfillChecksumsResponse reports a checksum backfill pass. Done is true
// when no unhashed objects remained after the pass, so a caller draining the
// backlog in batches knows when to stop.
type BackfillChecksumsResponse struct {
	Outcome
	Processed  int  `json:"processed"`
	Unreadable int  `json:"unreadable"`
	Done       bool `json:"done"`
}

// UnreadableCopy is one copy that is encrypted with no key.
type UnreadableCopy struct {
	Key       string    `json:"key"`
	Backend   string    `json:"backend"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// UnreadableListResponse lists copies that are encrypted with no key: up to
// the requested limit of them, and how many exist in total.
type UnreadableListResponse struct {
	Total  int64            `json:"total"`
	Copies []UnreadableCopy `json:"copies"`
}

// UnreadablePurgeResponse reports a purge of unreadable copies.
type UnreadablePurgeResponse struct {
	Outcome
	Purged int `json:"purged"`
	Failed int `json:"failed,omitempty"`
}

// ReconcileResponse reports a reconcile pass: objects adopted from backend
// storage into the ledger, ledger rows dropped for objects no longer present,
// and how many backends were walked.
type ReconcileResponse struct {
	Outcome
	Imported        int `json:"imported"`
	Removed         int `json:"removed"`
	BackendsScanned int `json:"backends_scanned"`
}

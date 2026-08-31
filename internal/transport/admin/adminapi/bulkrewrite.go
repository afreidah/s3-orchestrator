// -------------------------------------------------------------------------------
// Admin API - Shared Bulk Rewrite DTOs
//
// Author: Alex Freidah
//
// The wire shape every fleet-wide rewrite pass reports. Compression and
// encryption run the same driver over the same ledger and differ only in what
// they do to each object's bytes, so they report through one type rather than a
// near-copy each. Kept in the leaf adminapi package so the server, the web UI
// and the out-of-process client depend on one definition.
// -------------------------------------------------------------------------------

package adminapi

// BulkRewriteOutcome is the part every bulk rewrite pass reports identically:
// the terminal status and the three counts that partition what the pass saw.
// The per-operation responses embed it and add their own success count.
//
// Skipped is carried separately from Failed because a copy can be left alone on
// purpose - too incompressible to be worth encoding, or on a backend already at
// its usage limit - and folding those into failures would make a healthy run
// read as broken.
//
// The four operations name their success count differently on the wire
// (compressed, decompressed, encrypted, decrypted) even though each reports the
// same quantity. Those names are already published; sharing the rest of the
// shape is what stops the four from drifting apart further.
type BulkRewriteOutcome struct {
	Status  string `json:"status"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
	Total   int    `json:"total"`
}

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
// the terminal status and the counts that partition what the pass saw.
// The per-operation responses embed it and add their own success count.
//
// Skipped counts copies left alone on purpose (too incompressible, or on a
// backend at its usage limit), not failures. Changed counts copies rewritten
// on the backend but not recorded because a client wrote the key meanwhile;
// a non-zero value means the pass overlapped live traffic, not a fault.
type BulkRewriteOutcome struct {
	Status  string `json:"status"`
	Skipped int    `json:"skipped"`
	Changed int    `json:"changed"`
	Failed  int    `json:"failed"`
	Total   int    `json:"total"`
}

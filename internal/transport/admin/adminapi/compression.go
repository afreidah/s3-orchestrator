// -------------------------------------------------------------------------------
// Admin API - Bulk Compression DTOs
//
// Author: Alex Freidah
//
// Wire types for the bulk compression endpoints (compress-existing,
// decompress-existing) shared by the handler and its clients. Kept in the leaf
// adminapi package so the server and its out-of-process client depend on one
// definition and the JSON shape cannot drift.
// -------------------------------------------------------------------------------

package adminapi

// BulkCompressionOutcome is the part both compression passes report
// identically. It carries skipped alongside failed because a compression pass
// declines objects on purpose - too small, or too incompressible to be worth
// encoding - and an operator reading a run needs that separate from work that
// went wrong.
type BulkCompressionOutcome struct {
	Status  string `json:"status"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
	Total   int    `json:"total"`
}

// CompressExistingResponse reports a compress-existing pass: how many
// previously verbatim objects were stored as an encoding instead.
type CompressExistingResponse struct {
	BulkCompressionOutcome
	Compressed int `json:"compressed"`
}

// DecompressExistingResponse reports a decompress-existing pass: how many
// encoded objects were rewritten back to the bytes the client wrote.
type DecompressExistingResponse struct {
	BulkCompressionOutcome
	Decompressed int `json:"decompressed"`
}

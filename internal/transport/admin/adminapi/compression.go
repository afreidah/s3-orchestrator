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

// CompressExistingResponse reports a compress-existing pass: how many
// previously verbatim objects were stored as an encoding instead.
type CompressExistingResponse struct {
	BulkRewriteOutcome
	Compressed int `json:"compressed"`
}

// DecompressExistingResponse reports a decompress-existing pass: how many
// encoded objects were rewritten back to the bytes the client wrote.
type DecompressExistingResponse struct {
	BulkRewriteOutcome
	Decompressed int `json:"decompressed"`
}

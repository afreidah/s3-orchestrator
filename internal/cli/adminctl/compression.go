// -------------------------------------------------------------------------------
// Admin CLI - compression commands (compress-existing, decompress-existing)
//
// Author: Alex Freidah
//
// Bulk compression maintenance. Enabling compression only affects objects
// written afterwards, so compress-existing is what brings a fleet that already
// holds data under the feature; decompress-existing takes it back out. The
// server returns 400 when no codec is available.
// -------------------------------------------------------------------------------

package adminctl

// cmdCompressExisting implements `s3-orchestrator admin compress-existing
// [-max=N]`. Encodes every object currently stored verbatim, or the first N of
// them. Objects too incompressible to benefit are reported as skipped rather
// than failed.
func cmdCompressExisting(args []string, c *client) int {
	return runBulkRewrite(args, c, "compress-existing", "/admin/api/compress-existing")
}

// cmdDecompressExisting implements `s3-orchestrator admin decompress-existing
// [-max=N]`. Rewrites every encoded object back to the bytes the client wrote,
// or the first N of them.
func cmdDecompressExisting(args []string, c *client) int {
	return runBulkRewrite(args, c, "decompress-existing", "/admin/api/decompress-existing")
}

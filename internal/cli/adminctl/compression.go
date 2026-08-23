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

// cmdCompressExisting implements `s3-orchestrator admin compress-existing`.
// Encodes every object currently stored verbatim. Objects too small or too
// incompressible to benefit are reported as skipped rather than failed.
func cmdCompressExisting(_ []string, c *client) int {
	return c.post("/admin/api/compress-existing", "", nil)
}

// cmdDecompressExisting implements `s3-orchestrator admin decompress-existing`.
// Rewrites every encoded object back to the bytes the client wrote.
func cmdDecompressExisting(_ []string, c *client) int {
	return c.post("/admin/api/decompress-existing", "", nil)
}

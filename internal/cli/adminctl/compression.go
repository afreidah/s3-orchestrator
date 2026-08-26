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

import (
	"flag"
	"fmt"
)

// cmdCompressExisting implements `s3-orchestrator admin compress-existing
// [-max=N]`. Encodes every object currently stored verbatim, or the first N of
// them. Objects too incompressible to benefit are reported as skipped rather
// than failed.
func cmdCompressExisting(args []string, c *client) int {
	return runBulkCompression(args, c, "compress-existing", "/admin/api/compress-existing")
}

// cmdDecompressExisting implements `s3-orchestrator admin decompress-existing
// [-max=N]`. Rewrites every encoded object back to the bytes the client wrote,
// or the first N of them.
func cmdDecompressExisting(args []string, c *client) int {
	return runBulkCompression(args, c, "decompress-existing", "/admin/api/decompress-existing")
}

// runBulkCompression parses the shared flag set and posts one pass.
//
// -max converts part of a fleet and stops, which is what makes a fleet-sized
// conversion something an operator can spread across maintenance windows. It
// needs nothing carried between runs: a converted copy leaves the listing that
// selected it, and one declined on ratio is recorded so it leaves too, so the
// next run picks up where this one stopped rather than re-examining it.
func runBulkCompression(args []string, c *client, name, path string) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	maxObjects := fs.Int(flagMax, 0, "Stop after rewriting this many objects (0 = the whole fleet)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *maxObjects > 0 {
		path += fmt.Sprintf(fmtMax, *maxObjects)
	}
	return c.post(path, "", nil)
}

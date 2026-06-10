// -------------------------------------------------------------------------------
// Admin CLI - scrub
//
// Author: Alex Freidah
//
// Triggers one scrubber pass that random-samples objects and verifies their
// stored content_hash. -batch-size overrides the configured default for this
// single invocation.
// -------------------------------------------------------------------------------

package adminctl

import (
	"flag"
	"fmt"
	"net/http"
)

// cmdScrub implements `s3-orchestrator admin scrub [-batch-size=N]`.
func cmdScrub(args []string, c *client) int {
	fs := flag.NewFlagSet("scrub", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	batchSize := fs.Int(flagBatchSize, 0, "Number of objects to verify (0 = use server default)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	path := "/admin/api/scrub"
	if *batchSize > 0 {
		path += fmt.Sprintf(fmtBatchSize, *batchSize)
	}
	return c.stream(http.MethodPost, path, "")
}

// -------------------------------------------------------------------------------
// Admin CLI - backfill-checksums
//
// Author: Alex Freidah
//
// Computes and stores content_hash for objects predating the integrity feature.
// -max bounds the objects processed per run (0 drains all) so a single call
// fits the client timeout, and -delay-ms paces batches to avoid hammering
// backends.
// -------------------------------------------------------------------------------

package adminctl

import (
	"flag"
	"net/http"
	"net/url"
	"strconv"
)

// cmdBackfillChecksums implements `s3-orchestrator admin backfill-checksums
// [-batch-size=N] [-max=N] [-delay-ms=N]`.
func cmdBackfillChecksums(args []string, c *client) int {
	fs := flag.NewFlagSet("backfill-checksums", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	batchSize := fs.Int(flagBatchSize, 100, "Objects per batch")
	maxObjects := fs.Int("max", 0, "Cap objects processed this run (0 = drain entire backlog)")
	delayMs := fs.Int("delay-ms", 0, "Pause between batches in milliseconds (rate-limits backend reads)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	q := url.Values{}
	if *batchSize != 100 {
		q.Set("batch_size", strconv.Itoa(*batchSize))
	}
	if *maxObjects > 0 {
		q.Set("max", strconv.Itoa(*maxObjects))
	}
	if *delayMs > 0 {
		q.Set("delay_ms", strconv.Itoa(*delayMs))
	}
	path := "/admin/api/backfill-checksums"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	return c.stream(http.MethodPost, path, "")
}

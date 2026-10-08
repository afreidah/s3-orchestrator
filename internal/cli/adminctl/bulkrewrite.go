// -------------------------------------------------------------------------------
// Admin CLI - shared bulk-rewrite command runner
//
// Author: Alex Freidah
//
// The flag parsing and POST every fleet-wide rewrite command shares. Compression
// and encryption differ only in which endpoint they call, so they take the same
// flags rather than each growing its own set.
// -------------------------------------------------------------------------------

package adminctl

import (
	"flag"
	"net/url"
	"strconv"
)

// runBulkRewrite parses the shared flag set and posts one pass. -max stops
// after that many objects; repeated runs resume where the last one stopped,
// because converted and ratio-declined copies leave the selection. -backend
// limits the pass to one backend's copies.
func runBulkRewrite(args []string, c *client, name, path string) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	maxObjects := fs.Int(flagMax, 0, "Stop after rewriting this many objects (0 = the whole fleet)")
	backend := fs.String(flagBackend, "", usageBackend)
	if err := fs.Parse(args); err != nil {
		return 1
	}

	q := url.Values{}
	if *maxObjects > 0 {
		q.Set(flagMax, strconv.Itoa(*maxObjects))
	}
	if *backend != "" {
		q.Set(queryBackend, *backend)
	}
	return c.post(withQuery(path, q), "", nil)
}

// withQuery appends an encoded query to a path, leaving the path alone when
// nothing was set. Built rather than formatted because more than one parameter
// can now be present, and a second "?" is not a query string.
func withQuery(path string, q url.Values) string {
	if enc := q.Encode(); enc != "" {
		return path + "?" + enc
	}
	return path
}

// -------------------------------------------------------------------------------
// Admin CLI - cache commands (cache-flush, cache-stats, cache-invalidate,
// cache-invalidate-prefix)
//
// Author: Alex Freidah
//
// Operates on the in-memory object data cache: flush drops every entry,
// stats reports entry count/size/capacity, and the invalidate commands drop a
// single key or every key under a prefix. The server rejects an empty prefix
// with 400 so a missing parameter cannot wipe the whole cache by accident.
// -------------------------------------------------------------------------------

package adminctl

import (
	"flag"
	"fmt"
	"net/url"
)

// cmdCacheFlush implements `s3-orchestrator admin cache-flush`. Drops every
// entry; the server returns 503 when caching is disabled.
func cmdCacheFlush(_ []string, c *client) int {
	return c.post("/admin/api/cache/flush", "", nil)
}

// cmdCacheStats implements `s3-orchestrator admin cache-stats`. Reports the
// object data cache entry count, size, and capacity.
func cmdCacheStats(_ []string, c *client) int {
	return c.get("/admin/api/cache", nil)
}

// cmdCacheInvalidate implements `s3-orchestrator admin cache-invalidate
// -key=<key>`. Drops a single key; returns 200 even for unknown keys.
func cmdCacheInvalidate(args []string, c *client) int {
	fs := flag.NewFlagSet("cache-invalidate", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	key := fs.String("key", "", "Cache key to invalidate (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *key == "" {
		fmt.Fprintln(c.stderr, "error: -key is required")
		return 1
	}
	return c.delete("/admin/api/cache/keys/"+*key, nil)
}

// cmdCacheInvalidatePrefix implements `s3-orchestrator admin
// cache-invalidate-prefix -prefix=<prefix>`. Drops every cached key under the
// prefix; use cache-flush for a deliberate full flush.
func cmdCacheInvalidatePrefix(args []string, c *client) int {
	fs := flag.NewFlagSet("cache-invalidate-prefix", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	prefix := fs.String("prefix", "", "Cache key prefix to invalidate (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *prefix == "" {
		fmt.Fprintln(c.stderr, "error: -prefix is required")
		return 1
	}
	return c.delete("/admin/api/cache/prefix?prefix="+url.QueryEscape(*prefix), nil)
}

// -------------------------------------------------------------------------------
// Admin CLI - Entry Point, Target Resolution, and Dispatch
//
// Author: Alex Freidah
//
// CLI wrapper around the admin API endpoints. Resolves the target server
// address and admin token with the precedence flag -> environment
// ($S3O_ADMIN_ADDR / $S3O_ADMIN_TOKEN) -> config file, loading the config only
// when a value is still missing - so a local binary can target a remote
// instance with no server config. Responses render as human-readable text by
// default and as raw JSON when --json is passed.
// -------------------------------------------------------------------------------

// Package adminctl implements the `s3-orchestrator admin ...` family of
// subcommands. Each command is a thin HTTP client over the admin API
// exposed by the running server. Responses render as human-readable text by
// default; the global --json flag switches to raw JSON.
package adminctl

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"

	"github.com/afreidah/s3-orchestrator/internal/cli/admintarget"
	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/config"
)

// adminBackendsPath and related constants used by this package.
const (
	adminBackendsPath = "/admin/api/backends/"
	adminTokenHeader  = "X-Admin-Token"

	flagBatchSize = "batch-size"
	fmtBatchSize  = "?batch_size=%d"
	fmtError      = "error: %v\n"

	errBackendNameRequired = "error: backend name is required"
	drainSubpath           = "/drain"
)

// Run is the CLI entry point for `s3-orchestrator admin`. It parses the
// admin-level flags, resolves the target address and token (flag -> env ->
// config via admintarget.Resolve), then dispatches to a per-command handler.
// Returns the process exit code so the caller in cmd/ can os.Exit cleanly.
func Run(args []string, stdout, stderr io.Writer) int { // codecov:ignore -- CLI entry point
	fs := flag.NewFlagSet("admin", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "Path to config file (only loaded when -addr/-token or their env vars are unset)")
	addr := fs.String("addr", "", "Server address (overrides $S3O_ADMIN_ADDR and config)")
	tokenFlag := fs.String("token", "", "Admin API token (overrides $S3O_ADMIN_TOKEN and config)")
	jsonOut := fs.Bool("json", false, "Output raw JSON instead of human-readable text")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `Usage: s3-orchestrator admin [flags] <command>

Commands:
  status              Show backend health and circuit breaker state
  object-locations    List all copies of an object (requires -key)
  cleanup-queue       Show cleanup queue depth and pending items
  usage-flush         Force flush usage counters to database
  replicate           Trigger one replication cycle
  rebalance           Trigger one rebalance cycle to redistribute objects across backends
  over-replication    Show or clean over-replicated objects (use --execute to clean)
  log-level           View or set the runtime log level (use -set to change)
  drain               Start draining a backend (requires backend name arg)
  drain-status        Check drain progress (requires backend name arg)
  drain-cancel        Cancel an active drain (requires backend name arg)
  remove-backend      Remove a backend and its data (requires backend name arg, --purge to delete S3 objects)
  scrub               Trigger an on-demand integrity scrub cycle (-batch-size to override, -key to verify one object now)
  backfill-checksums  Compute and store content hashes for unhashed objects (use -max and -delay-ms to bound and pace each run)
  reconcile           Reconcile DB against backend (use -backend to scope to one backend)
  usage-reconcile     Recompute bytes_used from the object ledger to correct quota drift
  encrypt-existing    Encrypt all unencrypted objects in place (requires encryption enabled)
  decrypt-existing    Decrypt all encrypted objects back to plaintext (requires encryption enabled)
  rotate-encryption-key  Re-wrap all DEKs sealed with -old-key-id under the current primary key
  compress-existing   Store every uncompressed object as chunked zstd (requires a compression codec)
  decompress-existing Rewrite every compressed object back to the bytes the client wrote
  workers             Show background worker last-tick health
  reload-status       Show the outcome of the last SIGHUP config reload
  trace-snapshot      Download the flight-recorder trace ring buffer to a file (use -o)
  cache-flush         Drop every entry from the in-memory object data cache
  cache-stats         Show object data cache entries, size, and capacity
  cache-invalidate    Drop a single key from the in-memory object data cache (requires -key)
  cache-invalidate-prefix  Drop every cached key under a prefix (requires -prefix)

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if fs.NArg() == 0 || fs.Arg(0) == "help" {
		fs.Usage()
		return 0
	}

	baseAddr, token, err := admintarget.Resolve(*addr, *tokenFlag, func() (*config.Config, error) {
		return config.LoadConfig(*configPath)
	})
	if err != nil {
		fmt.Fprintf(stderr, fmtError, err)
		return 1
	}
	if baseAddr == "" {
		fmt.Fprintln(stderr, "error: server address required (set -addr, $S3O_ADMIN_ADDR, or server.listen_addr in config)")
		return 1
	}
	if token == "" {
		fmt.Fprintln(stderr, "error: admin token required (set -token, $S3O_ADMIN_TOKEN, or ui.admin_token/admin_key in config)")
		return 1
	}
	if !strings.HasPrefix(baseAddr, "http") {
		baseAddr = "http://" + baseAddr
	}

	return CommandWithFormat(fs.Arg(0), fs.Args()[1:], baseAddr, token, output.FromJSON(*jsonOut), stdout, stderr)
}

// -------------------------------------------------------------------------
// DISPATCH
// -------------------------------------------------------------------------

// handler is the shared shape for per-command handler functions.
type handler func(args []string, c *client) int

// handlers dispatches admin CLI subcommands. Adding a command is as simple
// as registering one entry here and adding its <command>.go file.
var handlers = map[string]handler{
	"status":                  cmdStatus,
	"object-locations":        cmdObjectLocations,
	"cleanup-queue":           cmdCleanupQueue,
	"cleanup-dlq":             cmdCleanupDLQ,
	"usage-flush":             cmdUsageFlush,
	"replicate":               cmdReplicate,
	"rebalance":               cmdRebalance,
	"over-replication":        cmdOverReplication,
	"log-level":               cmdLogLevel,
	"drain":                   cmdDrain,
	"drain-status":            cmdDrainStatus,
	"drain-cancel":            cmdDrainCancel,
	"scrub":                   cmdScrub,
	"backfill-checksums":      cmdBackfillChecksums,
	"remove-backend":          cmdRemoveBackend,
	"reconcile":               cmdReconcile,
	"usage-reconcile":         cmdUsageReconcile,
	"encrypt-existing":        cmdEncryptExisting,
	"decrypt-existing":        cmdDecryptExisting,
	"rotate-encryption-key":   cmdRotateEncryptionKey,
	"compress-existing":       cmdCompressExisting,
	"decompress-existing":     cmdDecompressExisting,
	"workers":                 cmdWorkers,
	"reload-status":           cmdReloadStatus,
	"trace-snapshot":          cmdTraceSnapshot,
	"cache-flush":             cmdCacheFlush,
	"cache-stats":             cmdCacheStats,
	"cache-invalidate":        cmdCacheInvalidate,
	"cache-invalidate-prefix": cmdCacheInvalidatePrefix,
}

// Command executes an admin CLI command in text output mode, returning the
// exit code. Exposed so tests can drive subcommands directly without parsing
// process-level flags.
func Command(cmd string, args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	return CommandWithFormat(cmd, args, baseAddr, token, output.FormatText, stdout, stderr)
}

// CommandWithFormat executes an admin CLI command with an explicit output
// format, returning the exit code. Run uses this with the format derived from
// the --json flag; Command wraps it with the text default.
func CommandWithFormat(cmd string, args []string, baseAddr, token string, format output.Format, stdout, stderr io.Writer) int {
	h, ok := handlers[cmd]
	if !ok {
		fmt.Fprintf(stderr, "unknown admin command: %s\n", cmd)
		return 1
	}
	return h(args, &client{
		api:    adminclient.New(baseAddr, token),
		format: format,
		stdout: stdout,
		stderr: stderr,
	})
}

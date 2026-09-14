// -------------------------------------------------------------------------------
// Admin CLI - Entry Point, Target Resolution, and Dispatch
//
// Author: Alex Freidah
//
// CLI wrapper around the admin API endpoints. Requests are SigV4-signed, so the
// keypair comes from the caller's flags or environment ($S3O_ACCESS_KEY_ID /
// $S3O_SECRET_ACCESS_KEY) and never from the server's config. The address
// resolves flag -> environment ($S3O_ADMIN_ADDR) -> config file, loading the
// config only when it is still missing, so a local binary can target a remote
// instance with no server config. Responses render as human-readable text by
// default and as raw JSON when --json is passed.
// -------------------------------------------------------------------------------

package adminctl

import (
	"cmp"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"

	"github.com/afreidah/s3-orchestrator/internal/cli/admintarget"
	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/config"
)

// The admin API route and header, plus the flag names, query formats and
// message literals the subcommands share.
const (
	adminBackendsPath = "/admin/api/backends/"

	flagBatchSize = "batch-size"
	fmtBatchSize  = "?batch_size=%d"
	flagMax       = "max"
	flagBackend   = "backend"
	queryBackend  = "backend"
	usageBackend  = "Restrict the pass to one backend (default: every backend)"
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
	configPath := fs.String("config", "config.yaml", "Path to config file (only loaded when -addr and its env var are unset)")
	addr := fs.String("addr", "", "Server address (overrides $S3O_ADMIN_ADDR and config)")
	accessKey := fs.String("access-key", "", "Access key ID to sign with (overrides $S3O_ACCESS_KEY_ID)")
	secretKey := fs.String("secret-key", "", "Secret access key to sign with (overrides $S3O_SECRET_ACCESS_KEY)")
	jsonOut := fs.Bool("json", false, "Output raw JSON instead of human-readable text")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `Usage: s3-orchestrator admin [flags] <command>

Commands:
  status              Show backend health and circuit breaker state
  object-locations    List all copies of an object (requires -key)
  object-tags         Read, replace or clear an object's tags (requires -key)
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
  scrub               Trigger an on-demand integrity scrub cycle (-batch-size to override, -key to verify one object now, -backend to scope)
  backfill-checksums  Compute and store content hashes for unhashed objects (use -max and -delay-ms to bound and pace each run, -backend to scope)
  reconcile           Reconcile DB against backend (use -backend to scope to one backend)
  lifecycle           Run one lifecycle expiration sweep now instead of waiting for the hourly tick
  usage-reconcile     Recompute bytes_used from the object ledger to correct quota drift
  encrypt-existing    Encrypt all unencrypted objects in place, or -max=N of them, -backend to scope (requires encryption enabled)
  decrypt-existing    Decrypt all encrypted objects back to plaintext, or -max=N of them, -backend to scope (requires encryption enabled)
  rotate-encryption-key  Re-wrap all DEKs sealed with -old-key-id under the current primary key
  compress-existing   Store every uncompressed object as chunked zstd, or -max=N of them, -backend to scope (requires a compression codec)
  decompress-existing Rewrite every compressed object back to the bytes the client wrote, or -max=N of them, -backend to scope
  workers             Show background worker last-tick health
  reload-status       Show the outcome of the last SIGHUP config reload
  trace-snapshot      Download the flight-recorder trace ring buffer to a file (use -o)
  cache-flush         Drop every entry from the in-memory object data cache
  cache-stats         Show object data cache entries, size, and capacity
  cache-invalidate    Drop a single key from the in-memory object data cache (requires -key)
  cache-invalidate-prefix  Drop every cached key under a prefix (requires -prefix)

Provisioning commands (each takes a verb; run one with no verb to list them):
  bucket              list, create or delete a virtual bucket
  user                list, create or delete an identity credentials belong to
  credential          list, issue or revoke a keypair
  grant               add or remove a user's access to a bucket

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

	creds := Credentials{
		AccessKeyID: cmp.Or(*accessKey, os.Getenv(admintarget.EnvAccessKey)),
		SecretKey:   cmp.Or(*secretKey, os.Getenv(admintarget.EnvSecretKey)),
	}
	if !creds.signs() {
		fmt.Fprintln(stderr, "error: a credential is required (set -access-key and -secret-key, "+
			"or $S3O_ACCESS_KEY_ID and $S3O_SECRET_ACCESS_KEY)")
		return 1
	}
	// The config file is only read when the address is still missing, which is
	// what lets a binary target a remote instance with three environment
	// variables and nothing on disk.
	baseAddr, err := admintarget.Resolve(*addr, func() (*config.Config, error) {
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
	// A bare host:port defaults to http, because the common target is a local
	// instance reached over a loopback or a private network. An operator
	// pointing at a remote one supplies the scheme, and https is preserved
	// exactly because the prefix check passes it through untouched.
	if !strings.HasPrefix(baseAddr, "http") {
		baseAddr = "http://" + baseAddr //nolint:gosec // NOSONAR S5332: scheme default for an operator-supplied address
	}

	return CommandWithFormat(fs.Arg(0), fs.Args()[1:], baseAddr, creds, output.FromJSON(*jsonOut), stdout, stderr)
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
	"object-tags":             cmdObjectTags,
	"cleanup-queue":           cmdCleanupQueue,
	"cleanup-dlq":             cmdCleanupDLQ,
	"usage-flush":             cmdUsageFlush,
	"replicate":               cmdReplicate,
	"rebalance":               cmdRebalance,
	"lifecycle":               cmdLifecycle,
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
	"bucket":                  cmdBucket,
	"user":                    cmdUser,
	"credential":              cmdCredential,
	"grant":                   cmdGrant,
}

// Command executes an admin CLI command in text output mode, returning the
// exit code. Exposed so tests can drive subcommands directly without parsing
// process-level flags.
func Command(cmd string, args []string, baseAddr string, creds Credentials, stdout, stderr io.Writer) int {
	return CommandWithFormat(cmd, args, baseAddr, creds, output.FormatText, stdout, stderr)
}

// Credentials is the keypair an admin CLI invocation signs with.
type Credentials struct {
	AccessKeyID string
	SecretKey   string
}

// signs reports whether these credentials carry a keypair to sign with.
func (c Credentials) signs() bool {
	return c.AccessKeyID != "" && c.SecretKey != ""
}

// CommandWithFormat executes one admin CLI command in an explicit output
// format, returning the exit code.
func CommandWithFormat(
	cmd string, args []string, baseAddr string, creds Credentials,
	format output.Format, stdout, stderr io.Writer,
) int {
	h, ok := handlers[cmd]
	if !ok {
		fmt.Fprintf(stderr, "unknown admin command: %s\n", cmd)
		return 1
	}
	api := adminclient.NewSigned(baseAddr, creds.AccessKeyID, creds.SecretKey)
	return h(args, &client{
		api:    api,
		format: format,
		stdout: stdout,
		stderr: stderr,
	})
}

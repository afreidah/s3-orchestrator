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
	"flag"
	"fmt"
	"io"
	"slices"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/cli/admintarget"
	"github.com/afreidah/s3-orchestrator/internal/cli/output"
)

// The flag names, query formats and message literals the subcommands share.
const (
	flagBatchSize = "batch-size"
	queryBatch    = "batch_size"
	flagMax       = "max"
	flagBackend   = "backend"
	queryBackend  = "backend"
	usageBackend  = "Restrict the pass to one backend (default: every backend)"
	fmtError      = "error: %v\n"

	errBackendNameRequired = "error: backend name is required"
)

// Run is the CLI entry point for `s3-orchestrator admin`. It parses the
// admin-level flags, resolves the signing keypair (flag -> env) and the target
// address (flag -> env -> config) through admintarget, then dispatches to a
// per-command handler.
// Returns the process exit code so the caller in cmd/ can os.Exit cleanly.
func Run(args []string, stdout, stderr io.Writer) int { // codecov:ignore -- CLI entry point
	fs := flag.NewFlagSet("admin", flag.ExitOnError)
	targetFlags := admintarget.RegisterFlags(fs)
	jsonOut := fs.Bool("json", false, "Output raw JSON instead of human-readable text")
	fs.SetOutput(stderr)
	fs.Usage = func() { writeUsage(stderr, fs) }
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if fs.NArg() == 0 || fs.Arg(0) == "help" {
		fs.Usage()
		return 0
	}

	target, err := targetFlags.Resolve()
	if err != nil {
		fmt.Fprintf(stderr, fmtError, err)
		return 1
	}
	return CommandWithFormat(fs.Arg(0), fs.Args()[1:], target, output.FromJSON(*jsonOut), stdout, stderr)
}

// -------------------------------------------------------------------------
// DISPATCH
// -------------------------------------------------------------------------

// handler is the shared shape for per-command handler functions.
type handler func(args []string, c *client) int

// adminCommands lists every admin subcommand in the order usage prints it.
// Dispatch and the usage text both read the two tables, so a command cannot
// be served without being documented.
var adminCommands = []verb{
	{"status", "Show backend health and circuit breaker state", cmdStatus},
	{"object-locations", "List all copies of an object (requires -key)", cmdObjectLocations},
	{"object-tags", "Read, replace or clear an object's tags (requires -key)", cmdObjectTags},
	{"cleanup-queue", "Show cleanup queue depth and pending items", cmdCleanupQueue},
	{"cleanup-dlq", "List dead-lettered cleanups, or requeue them (list|requeue, -backend to scope)", cmdCleanupDLQ},
	{"usage-flush", "Force flush usage counters to database", cmdUsageFlush},
	{"replicate", "Trigger one replication cycle", cmdReplicate},
	{"rebalance", "Trigger one rebalance cycle to redistribute objects across backends", cmdRebalance},
	{"over-replication", "Show or clean over-replicated objects (use --execute to clean)", cmdOverReplication},
	{"unreadable", "List copies encrypted with no key (use --execute to delete them)", cmdUnreadable},
	{"log-level", "View or set the runtime log level (use -set to change)", cmdLogLevel},
	{"drain", "Start draining a backend (requires backend name arg)", cmdDrain},
	{"drain-status", "Check drain progress (requires backend name arg)", cmdDrainStatus},
	{"drain-cancel", "Cancel an active drain (requires backend name arg)", cmdDrainCancel},
	{"cordon", "Stop new copies landing on a backend; reads continue (requires backend name arg)", cmdCordon},
	{"uncordon", "Let new copies land on a cordoned backend again (requires backend name arg)", cmdUncordon},
	{"remove-backend", "Remove a backend and its data (requires backend name arg, --purge to delete S3 objects)", cmdRemoveBackend},
	{"scrub", "Trigger an on-demand integrity scrub cycle (-batch-size to override, -key to verify one object now, -backend to scope)", cmdScrub},
	{"backfill-checksums", "Compute and store content hashes for unhashed objects (use -max and -delay-ms to bound and pace each run, -backend to scope)", cmdBackfillChecksums},
	{"reconcile", "Reconcile DB against backend (use -backend to scope to one backend)", cmdReconcile},
	{"lifecycle", "Run one lifecycle expiration sweep now instead of waiting for the hourly tick", cmdLifecycle},
	{"usage-reconcile", "Recompute bytes_used from the object ledger to correct quota drift", cmdUsageReconcile},
	{"encrypt-existing", "Encrypt all unencrypted objects, or -max=N of them, -backend to scope (requires encryption enabled)", cmdEncryptExisting},
	{"decrypt-existing", "Decrypt all encrypted objects back to plaintext, or -max=N of them, -backend to scope (requires encryption enabled)", cmdDecryptExisting},
	{"rotate-encryption-key", "Re-wrap all DEKs sealed with -old-key-id under the current primary key", cmdRotateEncryptionKey},
	{"compress-existing", "Store every uncompressed object as chunked zstd, or -max=N of them, -backend to scope (requires a compression codec)", cmdCompressExisting},
	{"decompress-existing", "Rewrite every compressed object back to the bytes the client wrote, or -max=N of them, -backend to scope", cmdDecompressExisting},
	{"workers", "Show background worker last-tick health across the running instances", cmdWorkers},
	{"instances", "Show every running instance's identity, log level, last reload and worker health", cmdInstances},
	{"reload-status", "Show the outcome of the last SIGHUP config reload", cmdReloadStatus},
	{"config", "Show the running configuration, with secrets redacted", cmdConfig},
	{"trace-snapshot", "Download the flight-recorder trace ring buffer to a file (use -o)", cmdTraceSnapshot},
	{"cache-flush", "Drop every entry from the in-memory object data cache", cmdCacheFlush},
	{"cache-stats", "Show object data cache entries, size, and capacity", cmdCacheStats},
	{"cache-invalidate", "Drop a single key from the in-memory object data cache (requires -key)", cmdCacheInvalidate},
	{"cache-invalidate-prefix", "Drop every cached key under a prefix (requires -prefix)", cmdCacheInvalidatePrefix},
}

// provisioningCommands are the nouns that take a verb, listed apart in usage
// because each prints its own verbs when run without one. Each summary is
// built from the noun's verb table.
var provisioningCommands = []verb{
	{"bucket", verbList(bucketVerbs) + " a virtual bucket", nounCommand("bucket", bucketVerbs)},
	{"user", verbList(userVerbs) + " an identity credentials belong to", nounCommand("user", userVerbs)},
	{"credential", verbList(credentialVerbs) + " a keypair", nounCommand("credential", credentialVerbs)},
	{"grant", verbList(grantVerbs) + " a user's access to a bucket, a backend or the orchestrator", nounCommand("grant", grantVerbs)},
}

// writeUsage prints the admin usage: every command, the provisioning
// commands, then the flags fs defines.
func writeUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(w, "Usage: s3-orchestrator admin [flags] <command>\n\nCommands:\n")
	writeVerbs(w, adminCommands)
	fmt.Fprint(w, "\nProvisioning commands (each takes a verb; run one with no verb to list them):\n")
	writeVerbs(w, provisioningCommands)
	fmt.Fprint(w, "\nFlags:\n")
	fs.PrintDefaults()
}

// findCommand returns the handler for a command name from either table.
func findCommand(name string) (handler, bool) {
	for _, table := range [][]verb{adminCommands, provisioningCommands} {
		if i := slices.IndexFunc(table, func(v verb) bool { return v.Name == name }); i >= 0 {
			return table[i].Run, true
		}
	}
	return nil, false
}

// CommandWithFormat executes one admin CLI command against target in an
// explicit output format, returning the exit code.
func CommandWithFormat(
	cmd string, args []string, target admintarget.Target,
	format output.Format, stdout, stderr io.Writer,
) int {
	h, ok := findCommand(cmd)
	if !ok {
		fmt.Fprintf(stderr, "unknown admin command: %s\n", cmd)
		return 1
	}
	api := adminclient.NewSigned(target.Addr, target.AccessKeyID, target.SecretKey)
	return h(args, &client{
		api:    api,
		format: format,
		stdout: stdout,
		stderr: stderr,
	})
}

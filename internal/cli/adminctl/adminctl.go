// -------------------------------------------------------------------------------
// Admin CLI - Operational Commands for a Running Instance
//
// Author: Alex Freidah
//
// CLI wrapper around the admin API endpoints. Reads config to discover the
// server address and admin token, then makes HTTP requests to the running
// instance. Formats JSON responses for human consumption.
// -------------------------------------------------------------------------------

// Package adminctl implements the `s3-orchestrator admin ...` family of
// subcommands. Each command is a thin HTTP client over the admin API
// exposed by the running server, formatting JSON responses for human
// consumption.
package adminctl

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// adminBackendsPath and related constants used by this package.
const (
	adminBackendsPath = "/admin/api/backends/"
	adminTokenHeader  = "X-Admin-Token"
	flagBatchSize     = "batch-size"
	fmtBatchSize      = "?batch_size=%d"
	fmtError          = "error: %v\n"

	errBackendNameRequired = "error: backend name is required"
	drainSubpath           = "/drain"
)

// Run is the CLI entry point for `s3-orchestrator admin`. It parses the
// admin-level flags, loads config, then dispatches to a per-command handler.
// Returns the process exit code so the caller in cmd/ can os.Exit cleanly.
func Run(args []string, stdout, stderr io.Writer) int { // codecov:ignore -- CLI entry point
	fs := flag.NewFlagSet("admin", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "Path to configuration file")
	addr := fs.String("addr", "", "Override server address (default: from config)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `Usage: s3-orchestrator admin [flags] <command>

Commands:
  status              Show backend health and circuit breaker state
  object-locations    List all copies of an object (requires -key)
  cleanup-queue       Show cleanup queue depth and pending items
  usage-flush         Force flush usage counters to database
  replicate           Trigger one replication cycle
  over-replication    Show or clean over-replicated objects (use --execute to clean)
  log-level           View or set the runtime log level (use -set to change)
  drain               Start draining a backend (requires backend name arg)
  drain-status        Check drain progress (requires backend name arg)
  drain-cancel        Cancel an active drain (requires backend name arg)
  remove-backend      Remove a backend and its data (requires backend name arg, --purge to delete S3 objects)
  scrub               Trigger an on-demand integrity scrub cycle (use -batch-size to override)
  backfill-checksums  Compute and store content hashes for all unhashed objects (use -batch-size to control pace)
  reconcile           Reconcile DB against backend (use -backend to scope to one backend)
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

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, fmtError, err)
		return 1
	}

	baseAddr := *addr
	if baseAddr == "" {
		baseAddr = cfg.Server.ListenAddr
	}
	if !strings.HasPrefix(baseAddr, "http") {
		baseAddr = "http://" + baseAddr
	}

	token := cfg.UI.AdminToken
	if token == "" {
		token = cfg.UI.AdminKey
	}
	if token == "" {
		fmt.Fprintln(stderr, "error: ui.admin_token or ui.admin_key is required in config for admin commands")
		return 1
	}

	return Command(fs.Arg(0), fs.Args()[1:], baseAddr, token, stdout, stderr)
}

// Command executes an admin CLI command, returning the exit code. Exposed so
// tests can drive subcommands directly without parsing process-level flags.
func Command(cmd string, args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	handler, ok := handlers[cmd]
	if !ok {
		fmt.Fprintf(stderr, "unknown admin command: %s\n", cmd)
		return 1
	}
	return handler(args, baseAddr, token, stdout, stderr)
}

// handler is the shared shape for per-command handler functions.
type handler func(args []string, baseAddr, token string, stdout, stderr io.Writer) int

// handlers dispatches admin CLI subcommands. Adding a command is as simple
// as registering one entry here.
var handlers = map[string]handler{
	"status":             cmdStatus,
	"object-locations":   cmdObjectLocations,
	"cleanup-queue":      cmdCleanupQueue,
	"usage-flush":        cmdUsageFlush,
	"replicate":          cmdReplicate,
	"over-replication":   cmdOverReplication,
	"log-level":          cmdLogLevel,
	"drain":              cmdDrain,
	"drain-status":       cmdDrainStatus,
	"drain-cancel":       cmdDrainCancel,
	"scrub":              cmdScrub,
	"backfill-checksums": cmdBackfillChecksums,
	"remove-backend":     cmdRemoveBackend,
	"reconcile":          cmdReconcile,
	"cache-flush":        cmdCacheFlush,
	"cache-stats":        cmdCacheStats,
	"cache-invalidate":         cmdCacheInvalidate,
	"cache-invalidate-prefix":  cmdCacheInvalidatePrefix,
}

// cmdStatus implements `s3-orchestrator admin status`. Issues a GET to
// /admin/api/status and prints the JSON status payload (per-backend
// health, quota usage, circuit-breaker states) to stdout.
func cmdStatus(_ []string, baseAddr, token string, stdout, stderr io.Writer) int {
	return doGet(baseAddr+"/admin/api/status", token, stdout, stderr)
}

// cmdObjectLocations implements `s3-orchestrator admin object-locations
// -key=<key>`. Looks up the per-backend ledger for one key so an
// operator can see exactly which backends hold a copy and at what size.
func cmdObjectLocations(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("object-locations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	key := fs.String("key", "", "Object key to look up (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *key == "" {
		fmt.Fprintln(stderr, "error: -key is required")
		return 1
	}
	return doGet(baseAddr+"/admin/api/object-locations?key="+*key, token, stdout, stderr)
}

// cmdCleanupQueue implements `s3-orchestrator admin cleanup-queue`.
// Returns the current pending-cleanup depth and a sample of pending
// items so an operator can spot stuck retries before they exhaust to
// the DLQ.
func cmdCleanupQueue(_ []string, baseAddr, token string, stdout, stderr io.Writer) int {
	return doGet(baseAddr+"/admin/api/cleanup-queue", token, stdout, stderr)
}

// cmdUsageFlush implements `s3-orchestrator admin usage-flush`. Triggers
// an out-of-band flush of the in-memory or Redis usage counters to
// backend_usage so dashboards reflect the latest deltas without waiting
// for the next periodic tick.
func cmdUsageFlush(_ []string, baseAddr, token string, stdout, stderr io.Writer) int {
	return doPost(baseAddr+"/admin/api/usage-flush", "", token, stdout, stderr)
}

// cmdCacheFlush implements `s3-orchestrator admin cache-flush`. Drops
// every entry from the in-memory object data cache; returns 503 from
// the server when caching is disabled. Used to reset cache state
// between cache-cold and cache-warm performance runs.
func cmdCacheFlush(_ []string, baseAddr, token string, stdout, stderr io.Writer) int {
	return doPost(baseAddr+"/admin/api/cache/flush", "", token, stdout, stderr)
}

// cmdCacheStats implements `s3-orchestrator admin cache-stats`. Reports
// the current object data cache entry count, size, and capacity for
// operators without direct Prometheus access.
func cmdCacheStats(_ []string, baseAddr, token string, stdout, stderr io.Writer) int {
	return doGet(baseAddr+"/admin/api/cache", token, stdout, stderr)
}

// cmdCacheInvalidate implements `s3-orchestrator admin cache-invalidate
// -key=<key>`. Drops a single key from the in-memory cache. Returns 200
// even for unknown keys, matching the cache's no-op invalidate contract.
func cmdCacheInvalidate(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cache-invalidate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	key := fs.String("key", "", "Cache key to invalidate (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *key == "" {
		fmt.Fprintln(stderr, "error: -key is required")
		return 1
	}
	return doDelete(baseAddr+"/admin/api/cache/keys/"+*key, token, stdout, stderr)
}

// cmdCacheInvalidatePrefix implements `s3-orchestrator admin
// cache-invalidate-prefix -prefix=<prefix>`. Drops every cached key
// under the prefix. Empty prefix is rejected by the server with 400
// to prevent accidental full-cache wipes via missing parameters;
// use cache-flush for the deliberate full-flush case.
func cmdCacheInvalidatePrefix(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cache-invalidate-prefix", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prefix := fs.String("prefix", "", "Cache key prefix to invalidate (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *prefix == "" {
		fmt.Fprintln(stderr, "error: -prefix is required")
		return 1
	}
	return doDelete(baseAddr+"/admin/api/cache/prefix?prefix="+url.QueryEscape(*prefix), token, stdout, stderr)
}

// cmdReplicate implements `s3-orchestrator admin replicate`. Triggers
// the replicator background worker on demand instead of waiting for the
// next scheduled tick. Useful right after a drain when the operator
// wants to converge replicas immediately.
func cmdReplicate(_ []string, baseAddr, token string, stdout, stderr io.Writer) int {
	return doPost(baseAddr+"/admin/api/replicate", "", token, stdout, stderr)
}

// cmdOverReplication implements `s3-orchestrator admin over-replication
// [-execute] [-batch-size=N]`. Without -execute it shows the current
// pending-excess count; with -execute it runs the over-replication
// cleaner once with an optional batch-size override.
func cmdOverReplication(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("over-replication", flag.ContinueOnError)
	fs.SetOutput(stderr)
	execute := fs.Bool("execute", false, "Run cleanup (default: show status only)")
	batchSize := fs.Int(flagBatchSize, 0, "Override batch size for cleanup")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *execute {
		url := baseAddr + "/admin/api/over-replication"
		if *batchSize > 0 {
			url += fmt.Sprintf(fmtBatchSize, *batchSize)
		}
		return doPost(url, "", token, stdout, stderr)
	}
	return doGet(baseAddr+"/admin/api/over-replication", token, stdout, stderr)
}

// cmdLogLevel implements `s3-orchestrator admin log-level [-set=LEVEL]`.
// Without -set it returns the current effective level; with -set it
// reconfigures the running instance's slog level (debug/info/warn/error)
// without a restart.
func cmdLogLevel(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("log-level", flag.ContinueOnError)
	fs.SetOutput(stderr)
	set := fs.String("set", "", "Set log level (debug, info, warn, error)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *set != "" {
		body := fmt.Sprintf(`{"level":%q}`, *set)
		return doPut(baseAddr+"/admin/api/log-level", body, token, stdout, stderr)
	}
	return doGet(baseAddr+"/admin/api/log-level", token, stdout, stderr)
}

// requireBackendName prints the missing-name error to stderr and returns
// true if args is empty (caller should bail with exit 1).
func requireBackendName(args []string, stderr io.Writer) bool {
	if len(args) == 0 {
		fmt.Fprintln(stderr, errBackendNameRequired)
		return true
	}
	return false
}

// cmdDrain implements `s3-orchestrator admin drain <backend>`. Starts
// a drain on the named backend: new writes are routed away while the
// drain worker migrates existing copies to other backends. Operator
// must follow up with drain-status until completion.
func cmdDrain(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	if requireBackendName(args, stderr) {
		return 1
	}
	return doPost(baseAddr+adminBackendsPath+args[0]+drainSubpath, "", token, stdout, stderr)
}

// cmdDrainStatus implements `s3-orchestrator admin drain-status
// <backend>`. Returns the in-flight drain progress (objects moved,
// bytes moved, errors) for the named backend.
func cmdDrainStatus(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	if requireBackendName(args, stderr) {
		return 1
	}
	return doGet(baseAddr+adminBackendsPath+args[0]+drainSubpath, token, stdout, stderr)
}

// cmdDrainCancel implements `s3-orchestrator admin drain-cancel
// <backend>`. Aborts an in-flight drain on the named backend; objects
// already migrated stay migrated, the rest stop where they are.
func cmdDrainCancel(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	if requireBackendName(args, stderr) {
		return 1
	}
	return doDelete(baseAddr+adminBackendsPath+args[0]+drainSubpath, token, stdout, stderr)
}

// cmdScrub implements `s3-orchestrator admin scrub [-batch-size=N]`.
// Triggers one scrubber pass that random-samples objects and verifies
// their content_hash. -batch-size overrides the configured default for
// this single invocation.
func cmdScrub(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scrub", flag.ContinueOnError)
	fs.SetOutput(stderr)
	batchSize := fs.Int(flagBatchSize, 0, "Number of objects to verify (0 = use server default)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	url := baseAddr + "/admin/api/scrub"
	if *batchSize > 0 {
		url += fmt.Sprintf(fmtBatchSize, *batchSize)
	}
	return doPost(url, "", token, stdout, stderr)
}

// cmdBackfillChecksums implements `s3-orchestrator admin backfill-
// checksums [-batch-size=N]`. Computes and stores content_hash for
// objects predating the integrity feature. Default batch size is 100
// per cycle to limit the per-call backend egress and DB write load.
func cmdBackfillChecksums(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backfill-checksums", flag.ContinueOnError)
	fs.SetOutput(stderr)
	batchSize := fs.Int(flagBatchSize, 100, "Objects per batch")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	url := baseAddr + "/admin/api/backfill-checksums"
	if *batchSize != 100 {
		url += fmt.Sprintf(fmtBatchSize, *batchSize)
	}
	return doPost(url, "", token, stdout, stderr)
}

// cmdReconcile implements `s3-orchestrator admin reconcile
// [-backend=NAME]`. Triggers an out-of-band reconcile pass that imports
// untracked objects and removes stale rows. Without -backend, reconciles
// every backend.
func cmdReconcile(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	backendName := fs.String("backend", "", "Scope reconcile to a single backend (default: all)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	url := baseAddr + "/admin/api/reconcile"
	if *backendName != "" {
		url += "?backend=" + *backendName
	}
	return doPost(url, "", token, stdout, stderr)
}

// cmdRemoveBackend implements `s3-orchestrator admin remove-backend
// <name> [-purge] [-confirm]`. Without -purge, only removes the metadata
// rows. With -purge but without -confirm, prints what would be deleted
// from the backend's S3 storage. With both, executes the destructive
// purge. The two-step flow guards against accidental data loss.
func cmdRemoveBackend(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("remove-backend", flag.ContinueOnError)
	fs.SetOutput(stderr)
	purge := fs.Bool("purge", false, "Also delete objects from the backend's S3 storage (requires --confirm)")
	confirm := fs.Bool("confirm", false, "Execute the purge (without this, --purge is a dry-run preview)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, errBackendNameRequired)
		return 1
	}
	name := fs.Arg(0)
	if !*purge {
		return doDelete(baseAddr+adminBackendsPath+name, token, stdout, stderr)
	}
	if !*confirm {
		return doRemovePreview(baseAddr, name, token, stdout, stderr)
	}
	return doRemovePurge(baseAddr, name, token, stdout, stderr)
}

// -------------------------------------------------------------------------
// REMOVE-BACKEND HELPERS
// -------------------------------------------------------------------------

// doRemovePreview calls the purge endpoint without confirmation and prints
// what would be destroyed.
func doRemovePreview(baseAddr, name, token string, stdout, stderr io.Writer) int {
	url := baseAddr + adminBackendsPath + name + "?purge=true"
	result, code := fetchJSONResponse(http.MethodDelete, url, token, stderr)
	if code != 0 {
		return code
	}

	objectCount, _ := result["object_count"].(float64)
	totalBytes, _ := result["total_bytes"].(float64)

	//nolint:gosec // G705: stdout print of admin-CLI response, not an HTML/HTTP write  -  no XSS surface
	fmt.Fprintf(stdout, "Backend %q contains %.0f objects (%.0f bytes).\n", name, objectCount, totalBytes)
	fmt.Fprintf(stdout, "This will permanently delete all objects from the backend's S3 storage and remove all database records.\n")
	fmt.Fprintf(stdout, "Re-run with --confirm to proceed.\n")
	return 0
}

// doRemovePurge performs the two-phase purge: gets a confirmation token from
// the preview endpoint, then executes with the token.
func doRemovePurge(baseAddr, name, token string, stdout, stderr io.Writer) int {
	url := baseAddr + adminBackendsPath + name + "?purge=true"
	result, code := fetchJSONResponse(http.MethodDelete, url, token, stderr)
	if code != 0 {
		return code
	}

	confirmToken, ok := result["confirm_token"].(string)
	if !ok || confirmToken == "" {
		fmt.Fprintf(stderr, "error: server did not return a confirmation token\n")
		return 1
	}
	return doDelete(url+"&confirm="+confirmToken, token, stdout, stderr)
}

// fetchJSONResponse issues an authenticated request and decodes the JSON
// body into a map. Returns (body, exitCode); a non-zero exitCode indicates
// the helper has already printed an error to stderr and the caller should
// propagate it. Shared between doRemovePreview and doRemovePurge so each
// only carries the response-shape handling unique to it.
func fetchJSONResponse(method, url, token string, stderr io.Writer) (map[string]any, int) {
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	if err != nil {
		fmt.Fprintf(stderr, fmtError, err)
		return nil, 1
	}
	req.Header.Set(adminTokenHeader, token)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req) //nolint:gosec // G704: admin CLI target address is user-provided via --addr flag
	if err != nil {
		fmt.Fprintf(stderr, fmtError, err)
		return nil, 1
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(stderr, "error: failed to parse response: %v\n", err)
		return nil, 1
	}
	return result, 0
}

// -------------------------------------------------------------------------
// HTTP HELPERS
// -------------------------------------------------------------------------

// doGet performs a GET against the admin API at url, prints the
// response body to stdout, and returns the process exit code (0 on
// success, non-zero on transport or HTTP error).
func doGet(url, token string, stdout, stderr io.Writer) int {
	return doRequest(http.MethodGet, url, "", token, stdout, stderr)
}

// doPost performs a POST against the admin API at url with the supplied
// body. Used for the trigger-style endpoints (replicate, scrub, drain,
// usage-flush) that have no useful response payload beyond status.
func doPost(url, body, token string, stdout, stderr io.Writer) int {
	return doRequest(http.MethodPost, url, body, token, stdout, stderr)
}

// doPut performs a PUT against the admin API at url with the supplied
// JSON body. Used for the configuration-update endpoints (notably
// log-level) where a full replacement is expected, not a delta.
func doPut(url, body, token string, stdout, stderr io.Writer) int {
	return doRequest(http.MethodPut, url, body, token, stdout, stderr)
}

// doDelete performs a DELETE against the admin API at url. Used for
// drain-cancel and remove-backend; the latter is a destructive
// operation gated by a separate -confirm flag at the caller level.
func doDelete(url, token string, stdout, stderr io.Writer) int {
	return doRequest(http.MethodDelete, url, "", token, stdout, stderr)
}

// doRequest is the shared HTTP transport for every admin verb. Sets the
// X-Admin-Token header for auth, the Content-Type header when a body is
// present, and a 30s client timeout so a hung server cannot stall the
// CLI indefinitely. Returns the process exit code.
func doRequest(method, url, body, token string, stdout, stderr io.Writer) int {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, url, bodyReader)
	if err != nil {
		fmt.Fprintf(stderr, fmtError, err)
		return 1
	}
	req.Header.Set(adminTokenHeader, token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req) //nolint:gosec // G704: admin CLI target address is user-provided via --addr flag
	if err != nil {
		fmt.Fprintf(stderr, fmtError, err)
		return 1
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "error reading response: %v\n", err)
		return 1
	}

	// Pretty-print JSON
	var pretty json.RawMessage
	if json.Unmarshal(data, &pretty) == nil {
		formatted, err := json.MarshalIndent(pretty, "", "  ")
		if err == nil {
			data = formatted
		}
	}

	fmt.Fprintln(stdout, string(data))

	if resp.StatusCode >= 400 {
		return 1
	}
	return 0
}
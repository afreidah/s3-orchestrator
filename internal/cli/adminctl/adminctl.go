// -------------------------------------------------------------------------------
// Admin CLI - Operational Commands for a Running Instance
//
// Author: Alex Freidah
//
// CLI wrapper around the admin API endpoints. Reads config to discover the
// server address and admin token, then makes HTTP requests to the running
// instance. Formats JSON responses for human consumption.
// -------------------------------------------------------------------------------

package adminctl

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

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
}

func cmdStatus(_ []string, baseAddr, token string, stdout, stderr io.Writer) int {
	return doGet(baseAddr+"/admin/api/status", token, stdout, stderr)
}

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

func cmdCleanupQueue(_ []string, baseAddr, token string, stdout, stderr io.Writer) int {
	return doGet(baseAddr+"/admin/api/cleanup-queue", token, stdout, stderr)
}

func cmdUsageFlush(_ []string, baseAddr, token string, stdout, stderr io.Writer) int {
	return doPost(baseAddr+"/admin/api/usage-flush", "", token, stdout, stderr)
}

func cmdReplicate(_ []string, baseAddr, token string, stdout, stderr io.Writer) int {
	return doPost(baseAddr+"/admin/api/replicate", "", token, stdout, stderr)
}

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

func cmdDrain(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	if requireBackendName(args, stderr) {
		return 1
	}
	return doPost(baseAddr+adminBackendsPath+args[0]+drainSubpath, "", token, stdout, stderr)
}

func cmdDrainStatus(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	if requireBackendName(args, stderr) {
		return 1
	}
	return doGet(baseAddr+adminBackendsPath+args[0]+drainSubpath, token, stdout, stderr)
}

func cmdDrainCancel(args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	if requireBackendName(args, stderr) {
		return 1
	}
	return doDelete(baseAddr+adminBackendsPath+args[0]+drainSubpath, token, stdout, stderr)
}

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

	//nolint:gosec // G705: stdout print of admin-CLI response, not an HTML/HTTP write — no XSS surface
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

func doGet(url, token string, stdout, stderr io.Writer) int {
	return doRequest(http.MethodGet, url, "", token, stdout, stderr)
}

func doPost(url, body, token string, stdout, stderr io.Writer) int {
	return doRequest(http.MethodPost, url, body, token, stdout, stderr)
}

func doPut(url, body, token string, stdout, stderr io.Writer) int {
	return doRequest(http.MethodPut, url, body, token, stdout, stderr)
}

func doDelete(url, token string, stdout, stderr io.Writer) int {
	return doRequest(http.MethodDelete, url, "", token, stdout, stderr)
}

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
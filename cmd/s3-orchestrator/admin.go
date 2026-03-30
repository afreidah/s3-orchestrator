// -------------------------------------------------------------------------------
// Admin Subcommand - CLI for Operational Tasks
//
// Author: Alex Freidah
//
// CLI wrapper around the admin API endpoints. Reads config to discover the
// server address and admin token, then makes HTTP requests to the running
// instance. Formats JSON responses for human consumption.
// -------------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

func runAdmin() { // codecov:ignore -- CLI entry point, delegates to adminCommand
	fs := flag.NewFlagSet("admin", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "Path to configuration file")
	addr := fs.String("addr", "", "Override server address (default: from config)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: s3-orchestrator admin [flags] <command>

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

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}

	if fs.NArg() == 0 || fs.Arg(0) == "help" {
		fs.Usage()
		os.Exit(0)
	}

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
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
		fmt.Fprintln(os.Stderr, "error: ui.admin_token or ui.admin_key is required in config for admin commands")
		os.Exit(1)
	}

	code := adminCommand(fs.Arg(0), fs.Args()[1:], baseAddr, token, os.Stdout, os.Stderr)
	os.Exit(code)
}

// adminCommand executes an admin CLI command, returning the exit code.
func adminCommand(cmd string, args []string, baseAddr, token string, stdout, stderr io.Writer) int {
	switch cmd {
	case "status":
		return doGet(baseAddr+"/admin/api/status", token, stdout, stderr)

	case "object-locations":
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

	case "cleanup-queue":
		return doGet(baseAddr+"/admin/api/cleanup-queue", token, stdout, stderr)

	case "usage-flush":
		return doPost(baseAddr+"/admin/api/usage-flush", "", token, stdout, stderr)

	case "replicate":
		return doPost(baseAddr+"/admin/api/replicate", "", token, stdout, stderr)

	case "over-replication":
		fs := flag.NewFlagSet("over-replication", flag.ContinueOnError)
		fs.SetOutput(stderr)
		execute := fs.Bool("execute", false, "Run cleanup (default: show status only)")
		batchSize := fs.Int("batch-size", 0, "Override batch size for cleanup")
		if err := fs.Parse(args); err != nil {
			return 1
		}
		if *execute {
			url := baseAddr + "/admin/api/over-replication"
			if *batchSize > 0 {
				url += fmt.Sprintf("?batch_size=%d", *batchSize)
			}
			return doPost(url, "", token, stdout, stderr)
		}
		return doGet(baseAddr+"/admin/api/over-replication", token, stdout, stderr)

	case "log-level":
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

	case "drain":
		if len(args) == 0 {
			fmt.Fprintln(stderr, "error: backend name is required")
			return 1
		}
		return doPost(baseAddr+"/admin/api/backends/"+args[0]+"/drain", "", token, stdout, stderr)

	case "drain-status":
		if len(args) == 0 {
			fmt.Fprintln(stderr, "error: backend name is required")
			return 1
		}
		return doGet(baseAddr+"/admin/api/backends/"+args[0]+"/drain", token, stdout, stderr)

	case "drain-cancel":
		if len(args) == 0 {
			fmt.Fprintln(stderr, "error: backend name is required")
			return 1
		}
		return doDelete(baseAddr+"/admin/api/backends/"+args[0]+"/drain", token, stdout, stderr)

	case "scrub":
		fs := flag.NewFlagSet("scrub", flag.ContinueOnError)
		fs.SetOutput(stderr)
		batchSize := fs.Int("batch-size", 0, "Number of objects to verify (0 = use server default)")
		if err := fs.Parse(args); err != nil {
			return 1
		}
		url := baseAddr + "/admin/api/scrub"
		if *batchSize > 0 {
			url += fmt.Sprintf("?batch_size=%d", *batchSize)
		}
		return doPost(url, "", token, stdout, stderr)

	case "backfill-checksums":
		fs := flag.NewFlagSet("backfill-checksums", flag.ContinueOnError)
		fs.SetOutput(stderr)
		batchSize := fs.Int("batch-size", 100, "Objects per batch")
		if err := fs.Parse(args); err != nil {
			return 1
		}
		url := baseAddr + "/admin/api/backfill-checksums"
		if *batchSize != 100 {
			url += fmt.Sprintf("?batch_size=%d", *batchSize)
		}
		return doPost(url, "", token, stdout, stderr)

	case "remove-backend":
		fs := flag.NewFlagSet("remove-backend", flag.ContinueOnError)
		fs.SetOutput(stderr)
		purge := fs.Bool("purge", false, "Also delete objects from the backend's S3 storage (requires --confirm)")
		confirm := fs.Bool("confirm", false, "Execute the purge (without this, --purge is a dry-run preview)")
		if err := fs.Parse(args); err != nil {
			return 1
		}
		if fs.NArg() == 0 {
			fmt.Fprintln(stderr, "error: backend name is required")
			return 1
		}
		name := fs.Arg(0)

		if !*purge {
			// Non-purge: remove DB records immediately
			return doDelete(baseAddr+"/admin/api/backends/"+name, token, stdout, stderr)
		}

		if !*confirm {
			// Purge dry-run: show what would be destroyed
			return doRemovePreview(baseAddr, name, token, stdout, stderr)
		}

		// Purge with confirmation: two-phase flow
		return doRemovePurge(baseAddr, name, token, stdout, stderr)

	default:
		fmt.Fprintf(stderr, "unknown admin command: %s\n", cmd)
		return 1
	}
}

// -------------------------------------------------------------------------
// REMOVE-BACKEND HELPERS
// -------------------------------------------------------------------------

// doRemovePreview calls the purge endpoint without confirmation and prints
// what would be destroyed.
func doRemovePreview(baseAddr, name, token string, stdout, stderr io.Writer) int {
	url := baseAddr + "/admin/api/backends/" + name + "?purge=true"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, url, nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	req.Header.Set("X-Admin-Token", token)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req) //nolint:gosec // G704: admin CLI target address is user-provided via --addr flag
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(stderr, "error: failed to parse response: %v\n", err)
		return 1
	}

	objectCount, _ := result["object_count"].(float64)
	totalBytes, _ := result["total_bytes"].(float64)

	fmt.Fprintf(stdout, "Backend %q contains %.0f objects (%.0f bytes).\n", name, objectCount, totalBytes)
	fmt.Fprintf(stdout, "This will permanently delete all objects from the backend's S3 storage and remove all database records.\n")
	fmt.Fprintf(stdout, "Re-run with --confirm to proceed.\n")
	return 0
}

// doRemovePurge performs the two-phase purge: gets a confirmation token from
// the preview endpoint, then executes with the token.
func doRemovePurge(baseAddr, name, token string, stdout, stderr io.Writer) int {
	// Phase 1: get confirmation token
	url := baseAddr + "/admin/api/backends/" + name + "?purge=true"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, url, nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	req.Header.Set("X-Admin-Token", token)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req) //nolint:gosec // G704: admin CLI target address is user-provided via --addr flag
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(stderr, "error: failed to parse response: %v\n", err)
		return 1
	}

	confirmToken, ok := result["confirm_token"].(string)
	if !ok || confirmToken == "" {
		fmt.Fprintf(stderr, "error: server did not return a confirmation token\n")
		return 1
	}

	// Phase 2: execute with confirmation token
	return doDelete(url+"&confirm="+confirmToken, token, stdout, stderr)
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
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	req.Header.Set("X-Admin-Token", token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req) //nolint:gosec // G704: admin CLI target address is user-provided via --addr flag
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
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

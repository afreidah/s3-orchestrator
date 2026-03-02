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
	"net/http"
	"os"
	"strings"

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
  log-level           View or set the runtime log level (use -set to change)

Flags:
`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])

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

	token := cfg.UI.AdminKey
	if token == "" {
		fmt.Fprintln(os.Stderr, "error: ui.admin_key is required in config for admin commands")
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

	default:
		fmt.Fprintf(stderr, "unknown admin command: %s\n", cmd)
		return 1
	}
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

func doRequest(method, url, body, token string, stdout, stderr io.Writer) int {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	req.Header.Set("X-Admin-Token", token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
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

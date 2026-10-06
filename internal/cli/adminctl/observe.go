// -------------------------------------------------------------------------------
// Admin CLI - observability commands (workers, reload-status, config, trace-snapshot)
//
// Author: Alex Freidah
//
// Read-only operator introspection: workers reports each background worker's
// last-tick health, reload-status reports the outcome of the last SIGHUP
// config reload, config prints the running configuration with secrets
// redacted, and trace-snapshot downloads the flight-recorder ring buffer
// to a file for `go tool trace`. workers/trace-snapshot return 503 when the
// worker pool or flight recorder is disabled.
// -------------------------------------------------------------------------------

package adminctl

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// defaultTraceFile is where trace-snapshot writes the ring buffer when -o is
// not supplied.
const defaultTraceFile = "trace.bin"

// cmdWorkers implements `s3-orchestrator admin workers`. Reports each
// background worker's last-tick health; returns 503 in proxy-only mode.
func cmdWorkers(_ []string, c *client) int {
	return c.get("/admin/api/workers", nil)
}

// cmdReloadStatus implements `s3-orchestrator admin reload-status`. Reports
// the outcome of the most recent SIGHUP config reload.
func cmdReloadStatus(_ []string, c *client) int {
	return c.get("/admin/api/reload-status", nil)
}

// cmdConfig implements `s3-orchestrator admin config`. Prints the running
// configuration, with secrets redacted.
func cmdConfig(_ []string, c *client) int {
	return c.get("/admin/api/config", renderConfig)
}

// renderConfig prints the log level and any fields waiting on a restart, then
// the configuration as YAML.
func renderConfig(w io.Writer, body []byte) error {
	var resp adminapi.ConfigResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	fmt.Fprintf(w, "# log level in effect: %s\n", resp.LogLevel)
	for _, field := range resp.PendingRestart {
		fmt.Fprintf(w, "# pending restart: %s\n", field)
	}
	_, err := io.WriteString(w, resp.Config)
	return err
}

// cmdTraceSnapshot implements `s3-orchestrator admin trace-snapshot -o=<file>`.
// Downloads the flight-recorder ring buffer (a binary `go tool trace` file)
// and writes it to disk. Returns 503 when the flight recorder is disabled.
func cmdTraceSnapshot(args []string, c *client) int {
	fs := flag.NewFlagSet("trace-snapshot", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	out := fs.String("o", defaultTraceFile, "Output file for the trace snapshot")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	data, status, code := c.request(http.MethodPost, "/admin/api/trace/snapshot", "")
	if code != 0 {
		return code
	}
	if status >= 400 {
		c.renderError(data)
		return 1
	}
	if err := os.WriteFile(*out, data, 0o600); err != nil {
		fmt.Fprintf(c.stderr, fmtError, err)
		return 1
	}
	fmt.Fprintf(c.stdout, "wrote %d bytes to %s\n", len(data), *out)
	return 0
}

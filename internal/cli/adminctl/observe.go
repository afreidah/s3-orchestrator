// -------------------------------------------------------------------------------
// Admin CLI - observability commands (workers, reload-status, trace-snapshot)
//
// Author: Alex Freidah
//
// Read-only operator introspection: workers reports each background worker's
// last-tick health, reload-status reports the outcome of the last SIGHUP
// config reload, and trace-snapshot downloads the flight-recorder ring buffer
// to a file for `go tool trace`. workers/trace-snapshot return 503 when the
// worker pool or flight recorder is disabled.
// -------------------------------------------------------------------------------

package adminctl

import (
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
)

// defaultTraceFile is where trace-snapshot writes the ring buffer when -o is
// not supplied.
const defaultTraceFile = "trace.bin"

// cmdWorkers implements `s3-orchestrator admin workers`. With no verb it
// reports each background worker's last-tick health; `run <name>` runs one
// worker now. Both return 503 in proxy-only mode.
func cmdWorkers(args []string, c *client) int {
	if len(args) == 0 {
		return c.get("/admin/api/workers", nil)
	}
	return nounCommand("workers", workerVerbs)(args, c)
}

// workerVerbs are the actions `admin workers` takes after its noun.
var workerVerbs = []verb{
	{Name: "run", Summary: "Run one tick of a worker now, streaming what it logs", Run: cmdRunWorker},
}

// cmdRunWorker implements `s3-orchestrator admin workers run <name>`. Runs
// one tick of the named worker through its advisory lock and streams each
// line it logs, then the outcome. A worker that is disabled, or that another
// instance is running, is reported as skipped.
func cmdRunWorker(args []string, c *client) int {
	if len(args) != 1 {
		fmt.Fprintln(c.stderr, "usage: s3-orchestrator admin workers run <name>")
		return 1
	}
	return c.stream(http.MethodPost, "/admin/api/workers/"+url.PathEscape(args[0])+"/run", "")
}

// cmdReloadStatus implements `s3-orchestrator admin reload-status`. Reports
// the outcome of the most recent SIGHUP config reload.
func cmdReloadStatus(_ []string, c *client) int {
	return c.get("/admin/api/reload-status", nil)
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

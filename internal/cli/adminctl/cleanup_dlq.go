// -------------------------------------------------------------------------------
// Admin CLI - cleanup-dlq
//
// Author: Alex Freidah
//
// Inspects and recovers the cleanup dead-letter queue. `cleanup-dlq list`
// shows the orphans whose backend deletes exhausted their retries (and why);
// `cleanup-dlq requeue` moves them back into the cleanup queue so the worker
// retries them against a recovered backend. Both accept -backend to scope to a
// single backend.
// -------------------------------------------------------------------------------

package adminctl

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"

	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdCleanupDLQ implements `s3-orchestrator admin cleanup-dlq [list|requeue]`.
// The first argument selects the subcommand (default: list); -backend scopes
// either to one backend.
func cmdCleanupDLQ(args []string, c *client) int {
	sub := "list"
	if len(args) > 0 && (args[0] == "list" || args[0] == "requeue") {
		sub, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("cleanup-dlq", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	backend := fs.String("backend", "", "Scope to a single backend (default: all)")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	q := url.Values{}
	if *backend != "" {
		q.Set("backend", *backend)
	}

	if sub == "requeue" {
		return c.post("/admin/api/cleanup-dlq/requeue?"+q.Encode(), "", renderCleanupDLQRequeue)
	}
	return c.get("/admin/api/cleanup-dlq?"+q.Encode(), renderCleanupDLQList)
}

// renderCleanupDLQList renders the DLQ listing as a table led by the backend,
// with the total depth beneath. Returning an error falls back to raw JSON.
func renderCleanupDLQList(w io.Writer, body []byte) error {
	var resp adminapi.CleanupDLQResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}

	headers := []string{"Backend", "Object key", "Reason", "Size", "Attempts", "Moved at", "Last error"}
	rows := make([][]string, len(resp.Items))
	for i := range resp.Items {
		it := &resp.Items[i]
		rows[i] = []string{
			it.Backend,
			it.ObjectKey,
			it.Reason,
			output.FormatBytes(it.SizeBytes),
			strconv.FormatInt(int64(it.Attempts), 10),
			it.MovedAt.Format("2006-01-02 15:04"),
			it.LastError,
		}
	}
	if err := output.Table(w, headers, rows); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\nDLQ depth: %d\n", resp.Depth)
	return err
}

// renderCleanupDLQRequeue reports how many rows the requeue moved back into the
// cleanup queue. Returning an error falls back to raw JSON.
func renderCleanupDLQRequeue(w io.Writer, body []byte) error {
	var resp adminapi.CleanupDLQRequeueResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	scope := "all backends"
	if resp.Backend != "" {
		scope = "backend " + resp.Backend
	}
	_, err := fmt.Fprintf(w, "Requeued %d cleanup row(s) for %s.\n", resp.Requeued, scope)
	return err
}

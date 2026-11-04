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
	"fmt"
	"io"
	"net/url"
	"strconv"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"
)

// cleanupDLQVerbs are the verbs under `admin cleanup-dlq`.
var cleanupDLQVerbs = []verb{
	{"list", "List the dead-lettered cleanups and why each failed", cleanupDLQList},
	{"requeue", "Move dead-lettered cleanups back into the queue", cleanupDLQRequeue},
}

// cleanupDLQList renders the dead-letter listing, scoped by -backend.
func cleanupDLQList(args []string, c *client) int {
	q, ok := cleanupDLQScope(args, c, "cleanup-dlq list")
	if !ok {
		return 1
	}
	return c.get(adminclient.WithQuery(adminapi.PathCleanupDLQ, q), renderCleanupDLQList)
}

// cleanupDLQRequeue moves dead-lettered rows back into the queue, scoped by
// -backend.
func cleanupDLQRequeue(args []string, c *client) int {
	q, ok := cleanupDLQScope(args, c, "cleanup-dlq requeue")
	if !ok {
		return 1
	}
	return c.post(adminclient.WithQuery(adminapi.PathCleanupDLQRequeue, q), renderCleanupDLQRequeue)
}

// cleanupDLQScope parses the -backend flag both verbs take into the query.
func cleanupDLQScope(args []string, c *client, name string) (url.Values, bool) {
	fs := c.flags(name)
	backend := fs.String(flagBackend, "", "Scope to a single backend (default: all)")
	if !c.parse(fs, args) {
		return nil, false
	}
	q := url.Values{}
	setIf(q, adminapi.ParamBackend, *backend)
	return q, true
}

// renderCleanupDLQList renders the DLQ listing as a table led by the backend,
// with the total depth beneath.
var renderCleanupDLQList = typed(func(w io.Writer, resp *adminapi.CleanupDLQResponse) error {
	headers := []string{"Backend", "Object key", "Reason", "Size", "Attempts", "Moved at", "Last error"}
	rows := make([][]string, len(resp.Items))
	for i := range resp.Items {
		it := &resp.Items[i]
		rows[i] = []string{
			it.Backend,
			it.ObjectKey,
			it.Reason,
			humanize.Bytes(it.SizeBytes),
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
})

// renderCleanupDLQRequeue reports how many rows the requeue moved back into the
// cleanup queue.
var renderCleanupDLQRequeue = typed(func(w io.Writer, resp *adminapi.CleanupDLQRequeueResponse) error {
	scope := "all backends"
	if resp.Backend != "" {
		scope = "backend " + resp.Backend
	}
	_, err := fmt.Fprintf(w, "Requeued %s for %s.\n", humanize.Count(int64(resp.Requeued), "cleanup row", "cleanup rows"), scope)
	return err
})

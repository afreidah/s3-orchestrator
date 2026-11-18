// -------------------------------------------------------------------------------
// Admin CLI - unhashed and unreadable copy listings
//
// Author: Alex Freidah
//
// Lists the copies the integrity passes act on: unhashed lists the copies with
// no content hash, scoped by backend and by whether the orchestrator manages
// them, and unreadable lists the copies encrypted with no key, or deletes all
// of them when -execute is passed. Both listings render as one table with the
// total beneath it.
// -------------------------------------------------------------------------------

package adminctl

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"
)

// usageListLimit describes the -limit flag both listings take.
const usageListLimit = "Maximum copies to list (0 = the server default)"

// cmdUnhashed implements `s3-orchestrator admin unhashed [-backend=NAME]
// [-managed=true|false] [-limit=N]`. Without -managed it lists managed and
// unmanaged copies alike.
func cmdUnhashed(args []string, c *client) int {
	fs := c.flags("unhashed")
	backend := fs.String(flagBackend, "", "Restrict the listing to one backend (default: every backend)")
	managed := fs.String("managed", "", "true for the copies backfill hashes, false for the unmanaged ones (default: both)")
	limit := fs.Int("limit", 0, usageListLimit)
	if !c.parse(fs, args) {
		return 1
	}
	q := url.Values{}
	setIf(q, adminapi.ParamBackend, *backend)
	setIf(q, adminapi.ParamManaged, *managed)
	setPositive(q, adminapi.ParamLimit, *limit)
	return c.get(adminclient.WithQuery(adminapi.PathUnhashed, q), renderCopyList)
}

// cmdUnreadable implements `s3-orchestrator admin unreadable [-limit=N]
// [-execute] [-batch-size=N]`.
func cmdUnreadable(args []string, c *client) int {
	fs := c.flags("unreadable")
	execute := fs.Bool("execute", false, "Delete every unreadable copy (default: list them)")
	limit := fs.Int("limit", 0, usageListLimit)
	batchSize := fs.Int(flagBatchSize, 0, "Copies purged per pass")
	if !c.parse(fs, args) {
		return 1
	}
	q := url.Values{}
	if *execute {
		setPositive(q, adminapi.ParamBatchSize, *batchSize)
		return c.stream(http.MethodPost, adminclient.WithQuery(adminapi.PathUnreadable, q))
	}
	setPositive(q, adminapi.ParamLimit, *limit)
	return c.get(adminclient.WithQuery(adminapi.PathUnreadable, q), renderCopyList)
}

// renderCopyList renders a copy listing as a table led by the key, with how
// many of the selected copies it shows beneath.
var renderCopyList = typed(func(w io.Writer, resp *adminapi.CopyListResponse) error {
	headers := []string{"Object key", "Backend", "Size", "Managed", "Encrypted", "Created"}
	rows := make([][]string, len(resp.Copies))
	for i := range resp.Copies {
		c := &resp.Copies[i]
		rows[i] = []string{
			c.Key,
			c.Backend,
			humanize.Bytes(c.SizeBytes),
			strconv.FormatBool(c.Managed),
			strconv.FormatBool(c.Encrypted),
			c.CreatedAt.Format("2006-01-02 15:04"),
		}
	}
	if err := output.Table(w, headers, rows); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\nShowing %d of %s.\n", len(resp.Copies), humanize.Count(resp.Total, "copy", "copies"))
	return err
})

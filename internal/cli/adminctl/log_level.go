// -------------------------------------------------------------------------------
// Admin CLI - log-level
//
// Author: Alex Freidah
//
// Views the current effective log level, or reconfigures the running instance's
// slog level (debug/info/warn/error) without a restart when -set is passed.
// -------------------------------------------------------------------------------

package adminctl

import (
	"flag"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdLogLevel implements `s3-orchestrator admin log-level [-set=LEVEL]`.
func cmdLogLevel(args []string, c *client) int {
	fs := flag.NewFlagSet("log-level", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	set := fs.String("set", "", "Set log level (debug, info, warn, error)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *set != "" {
		return c.sendJSON(http.MethodPut, adminapi.PathLogLevel, adminapi.SetLogLevelRequest{Level: *set}, nil)
	}
	return c.get(adminapi.PathLogLevel, nil)
}

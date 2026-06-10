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
	"fmt"
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
		body := fmt.Sprintf(`{"level":%q}`, *set)
		return c.put("/admin/api/log-level", body, nil)
	}
	return c.get("/admin/api/log-level", nil)
}

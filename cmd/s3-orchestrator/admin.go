// -------------------------------------------------------------------------------
// Admin Subcommand Shim
//
// Author: Alex Freidah
//
// Thin entry point that delegates to internal/cli/adminctl. The real
// implementation lives there so cmd/ stays minimal.
// -------------------------------------------------------------------------------

package main

import (
	"os"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminctl"
)

// runAdmin parses os.Args and dispatches to the admin CLI implementation.
func runAdmin() { // codecov:ignore -- thin wrapper around adminctl.Run
	os.Exit(adminctl.Run(os.Args[1:], os.Stdout, os.Stderr))
}

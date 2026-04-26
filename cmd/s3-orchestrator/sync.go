// -------------------------------------------------------------------------------
// Sync Subcommand Shim
//
// Author: Alex Freidah
//
// Thin entry point that delegates to internal/cli/synccmd.
// -------------------------------------------------------------------------------

package main

import (
	"os"

	"github.com/afreidah/s3-orchestrator/internal/cli/synccmd"
)

// runSync parses os.Args and dispatches to the sync CLI implementation.
func runSync() { // codecov:ignore -- thin wrapper around synccmd.Run
	os.Exit(synccmd.Run(os.Args[1:], os.Stderr))
}
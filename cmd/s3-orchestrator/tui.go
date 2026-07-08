// -------------------------------------------------------------------------------
// TUI Subcommand Shim
//
// Author: Alex Freidah
//
// Thin entry point that delegates to internal/cli/tui. The real
// implementation lives there so cmd/ stays minimal.
// -------------------------------------------------------------------------------

package main

import (
	"os"

	"github.com/afreidah/s3-orchestrator/internal/cli/tui"
)

// runTUI parses os.Args and dispatches to the TUI implementation.
func runTUI() { // codecov:ignore -- thin wrapper around tui.Run
	os.Exit(tui.Run(os.Args[1:], os.Stdout, os.Stderr))
}

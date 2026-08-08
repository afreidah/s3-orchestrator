// -------------------------------------------------------------------------------
// Init Subcommand Shim
//
// Author: Alex Freidah
//
// Thin entry point that delegates to internal/cli/initcmd.
// -------------------------------------------------------------------------------

package main

import (
	"os"

	"github.com/afreidah/s3-orchestrator/internal/cli/initcmd"
)

// runInit parses os.Args and dispatches to the init CLI implementation.
func runInit() { // codecov:ignore -- thin wrapper around initcmd.Run
	os.Exit(initcmd.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

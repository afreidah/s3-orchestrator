// -------------------------------------------------------------------------------
// Validate Subcommand - Check Configuration File
//
// Author: Alex Freidah
//
// Thin shim into internal/cli/validatecmd, which loads and validates a
// configuration file without starting the server.
// -------------------------------------------------------------------------------

package main

import (
	"os"

	"github.com/afreidah/s3-orchestrator/internal/cli/validatecmd"
)

// runValidate dispatches into validatecmd.Run and exits with its status code.
func runValidate() { // codecov:ignore -- thin wrapper around validatecmd.Run
	os.Exit(validatecmd.Run(os.Args[1:], os.Stdout, os.Stderr))
}

// -------------------------------------------------------------------------------
// Version Subcommand - Print Build Information
//
// Author: Alex Freidah
//
// Prints the binary version, Go version, and target platform, then exits.
// The version is set at build time via -ldflags.
// -------------------------------------------------------------------------------

package main

import (
	"fmt"
	"runtime"

	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// runVersion prints the binary version, Go version, and platform to stdout.
func runVersion() { // codecov:ignore -- trivial fmt.Printf, no branching logic
	fmt.Printf("s3-orchestrator %s %s %s/%s\n",
		telemetry.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

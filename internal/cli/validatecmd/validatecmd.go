// -------------------------------------------------------------------------------
// Validate CLI - Check Configuration File
//
// Author: Alex Freidah
//
// Loads and validates a configuration file without starting the server, printing
// a brief summary on success. Lives here (not in cmd/) so the load + format
// logic is unit-testable without spawning the binary; cmd/ keeps only the
// os.Exit wrapper.
// -------------------------------------------------------------------------------

// Package validatecmd implements the `s3-orchestrator validate` subcommand,
// which checks a configuration file and reports a summary without starting
// the server.
package validatecmd

import (
	"flag"
	"fmt"
	"io"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// Run parses the validate flags, validates the configuration file, and returns
// the process exit code: 0 when the config is valid, 1 otherwise.
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "Path to configuration file")
	_ = fs.Parse(args)

	if err := validate(*configPath, stdout); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// validate loads and validates the configuration file at path, writing a
// summary to w on success or returning the validation error.
func validate(path string, w io.Writer) error {
	cfg, err := config.LoadConfig(path)
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "config %s: valid\n", path)
	fmt.Fprintf(w, "  backends: %d\n", len(cfg.Backends))
	fmt.Fprintf(w, "  buckets:  %d\n", len(cfg.Buckets))
	fmt.Fprintf(w, "  routing:  %s\n", cfg.RoutingStrategy)
	return nil
}

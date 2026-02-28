// -------------------------------------------------------------------------------
// Validate Subcommand - Check Configuration File
//
// Author: Alex Freidah
//
// Loads and validates a configuration file without starting the server. Exits 0
// on success with a brief summary, or exits 1 with validation errors.
// -------------------------------------------------------------------------------

package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// runValidate parses flags and delegates to validateConfig, exiting with the
// appropriate status code.
func runValidate() { // codecov:ignore -- os.Exit wrapper, logic tested via validateConfig
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "Path to configuration file")
	_ = fs.Parse(os.Args[1:])

	if err := validateConfig(*configPath, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// validateConfig loads and validates a configuration file, writing a summary to
// the given writer on success or returning the validation error.
func validateConfig(path string, w io.Writer) error {
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

// -------------------------------------------------------------------------------
// S3 Orchestrator - Unified S3 Endpoint with Quota Management
//
// Author: Alex Freidah
//
// Entry point for the S3 proxy service. Dispatches to subcommands. The actual
// implementations live under internal/cli/* so cmd/ stays a thin shell over
// signal handling and flag parsing.
// -------------------------------------------------------------------------------

// Package main is the s3-orchestrator binary entry point. It dispatches
// to subcommands implemented under internal/cli and wraps process exit
// so library code never calls os.Exit directly.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/afreidah/s3-orchestrator/internal/cli/serve"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// main is the program entry point.
func main() { // codecov:ignore -- thin wrapper, logic tested via subcommands
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "sync":
			os.Args = os.Args[1:]
			runSync()
			return
		case "version":
			runVersion()
			return
		case "validate":
			os.Args = os.Args[1:]
			runValidate()
			return
		case "init":
			os.Args = os.Args[1:]
			runInit()
			return
		case "admin":
			os.Args = os.Args[1:]
			runAdmin()
			return
		case "help", "--help", "-h":
			printUsage()
			return
		}
	}
	runServe()
}

// printUsage writes the top-level command summary to stderr. Called when
// the binary is invoked with help, --help, -h, or no arguments.
func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: s3-orchestrator [command]

Commands:
  (default)   Start the S3 proxy server
  init        Generate a configuration file interactively
  admin       Operational CLI for a running instance
  sync        Import pre-existing bucket objects into the database
  validate    Check a configuration file without starting the server
  version     Print version and build info
  help        Show this help message

Run 's3-orchestrator <command> --help' for command-specific flags.
`)
}

// runServe parses the serve-mode flags (config path, operating mode) and
// dispatches into the server bootstrap. It is the default entry point
// when no subcommand is supplied. The codecov:ignore is set because this
// is an os.Exit-bearing wrapper; the real logic lives in cli/serve.
func runServe() { // codecov:ignore -- flag parsing + os.Exit wrapper
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	mode := flag.String("mode", "all", "Operating mode: api, worker, or all")
	flag.Parse()

	switch *mode {
	case "api", "worker", "all":
	default:
		fmt.Fprintf(os.Stderr, "invalid mode %q: must be api, worker, or all\n", *mode)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := serve.Run(ctx, *configPath, *mode, os.Stdout); err != nil {
		slog.ErrorContext(ctx, "server error", logfmt.Err(err))
		os.Exit(1)
	}
}

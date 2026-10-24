// -------------------------------------------------------------------------------
// S3 Orchestrator - Unified S3 Endpoint with Quota Management
//
// Author: Alex Freidah
//
// Entry point for the S3 proxy service. Dispatches to subcommands. The actual
// implementations live under internal/cli/* so cmd/ stays a thin shell over
// signal handling and flag parsing.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminctl"
	"github.com/afreidah/s3-orchestrator/internal/cli/initcmd"
	"github.com/afreidah/s3-orchestrator/internal/cli/serve"
	"github.com/afreidah/s3-orchestrator/internal/cli/synccmd"
	"github.com/afreidah/s3-orchestrator/internal/cli/tui"
	"github.com/afreidah/s3-orchestrator/internal/cli/validatecmd"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// commands maps each subcommand to the function that runs it with the
// arguments after its name and returns the process exit code.
var commands = map[string]func(args []string) int{
	"admin":    func(args []string) int { return adminctl.Run(args, os.Stdout, os.Stderr) },
	"init":     func(args []string) int { return initcmd.Run(args, os.Stdin, os.Stdout, os.Stderr) },
	"sync":     func(args []string) int { return synccmd.Run(args, os.Stderr) },
	"tui":      func(args []string) int { return tui.Run(args, os.Stdout, os.Stderr) },
	"validate": func(args []string) int { return validatecmd.Run(args, os.Stdout, os.Stderr) },
	"version":  runVersion,
}

// main is the program entry point.
func main() { // codecov:ignore -- thin wrapper, logic tested via subcommands
	if len(os.Args) > 1 {
		if run, ok := commands[os.Args[1]]; ok {
			os.Exit(run(os.Args[2:]))
		}
		switch os.Args[1] {
		case "help", "--help", "-h":
			printUsage()
			return
		}
	}
	runServe()
}

// -------------------------------------------------------------------------
// USAGE AND SERVE
// -------------------------------------------------------------------------

// printUsage writes the top-level command summary to stderr. Called when
// the binary is invoked with help, --help or -h.
func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: s3-orchestrator [command]

Commands:
  (default)   Start the S3 proxy server
  init        Generate a configuration file interactively
  admin       Operational CLI for a running instance
  tui         Interactive terminal dashboard for a running instance
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
	modeFlag := flag.String("mode", "all", "Operating mode: api, worker, or all")
	flag.Parse()

	mode, err := config.ParseMode(*modeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := serve.Run(ctx, *configPath, mode, os.Stdout); err != nil {
		slog.ErrorContext(ctx, "server error",
			logfmt.Component("main"),
			"error", err,
		)
		os.Exit(1)
	}
}

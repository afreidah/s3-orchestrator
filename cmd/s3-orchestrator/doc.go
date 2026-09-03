// Package main is the s3-orchestrator binary entry point. It dispatches
// to subcommands implemented under internal/cli and wraps process exit
// so library code never calls os.Exit directly.
package main

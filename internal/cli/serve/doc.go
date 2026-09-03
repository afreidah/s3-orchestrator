// Package serve implements the `s3-orchestrator serve` subcommand. It is
// a thin composition root: load the configuration file, construct a
// runtime.Runtime, and run it. All daemon assembly lives in
// internal/runtime.
package serve

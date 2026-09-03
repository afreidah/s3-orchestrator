// Package adminctl implements the `s3-orchestrator admin ...` family of
// subcommands. Each command is a thin HTTP client over the admin API
// exposed by the running server. Responses render as human-readable text by
// default; the global --json flag switches to raw JSON.
package adminctl

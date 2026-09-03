// Package adminstream defines the NDJSON progress-event contract shared by the
// admin API server and its clients. The server emits one Event per line with
// the ContentType media type; the client decodes line by line and renders
// incremental progress.
//
// A leaf package with no app-specific dependencies, so both the transport and
// CLI layers can import it without a cycle.
package adminstream

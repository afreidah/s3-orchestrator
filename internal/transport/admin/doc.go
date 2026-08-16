// Package admin implements the token-authenticated admin HTTP API mounted under
// /admin/api. It splits into read-only observability endpoints (status,
// object-locations, cleanup-queue, worker health, reload-status) and control
// endpoints (object read and write, backend add/remove, usage-flush and
// reconcile, log-level, and the key-rotation and encrypt/decrypt-existing
// operations).
//
// The work itself lives in internal/ops. A handler here parses the request,
// calls the matching operation, and renders what it reports: counts as JSON,
// a declined run as a "skipped" outcome carrying the reason, and a failure as
// an error status. The same operations back the web UI, so an action behaves
// identically whichever interface an operator drives it from.
//
// Responses are shared wire types from the adminapi subpackage so the server
// and its out-of-process clients (adminctl, the TUI) cannot drift on JSON
// shape, and secret material such as raw envelope keys is never serialized
// onto the wire.
package admin

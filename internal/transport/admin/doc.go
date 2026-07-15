// Package admin implements the token-authenticated admin HTTP API mounted under
// /admin/api. It splits into read-only observability endpoints (status,
// object-locations, cleanup-queue, worker health, reload-status) and control
// endpoints (backend add/remove, usage-flush and reconcile, log-level, and the
// key-rotation and encrypt/decrypt-existing operations).
//
// The handler depends on small consumer-defined interfaces for every
// collaborator it calls - the backend manager, the workers, and the metadata
// store - so this transport package does not import internal/worker or the
// concrete store implementations; the real types satisfy the interfaces
// structurally. Responses are shared wire types from the adminapi subpackage so
// the server and its out-of-process clients (adminctl, the TUI) cannot drift on
// JSON shape, and secret material such as raw envelope keys is never
// serialized onto the wire.
package admin

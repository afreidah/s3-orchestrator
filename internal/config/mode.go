// -------------------------------------------------------------------------------
// Daemon Operating Mode
//
// Author: Alex Freidah
//
// One typed constant for the --mode flag value. runtime, di, httpserver, and
// the CLI all consume this rather than untyped strings so typos become
// compile errors and the valid values live in one place.
// -------------------------------------------------------------------------------

package config

import "fmt"

// Mode selects which subsystems the daemon brings up.
//
//   - ModeAPI: HTTP server + S3 / UI / admin handlers. No background workers.
//   - ModeWorker: background workers only (rebalancer, replicator, cleanup,
//     scrubber, reconciler, lifecycle, pending reaper). No HTTP serving
//     beyond health endpoints.
//   - ModeAll: both, single-instance deployment.
type Mode string

// Mode values accepted by the --mode flag.
const (
	ModeAPI    Mode = "api"
	ModeWorker Mode = "worker"
	ModeAll    Mode = "all"
)

// ParseMode validates a raw mode string and returns the typed constant.
// Empty input is rejected so a caller does not silently fall back to a
// default — the CLI provides its own default before reaching this point.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeAPI, ModeWorker, ModeAll:
		return Mode(s), nil
	default:
		return "", fmt.Errorf("invalid mode %q: must be one of api, worker, all", s)
	}
}

// IsAPI reports whether the mode brings up the API surface.
func (m Mode) IsAPI() bool { return m == ModeAPI || m == ModeAll }

// IsWorker reports whether the mode brings up background workers.
func (m Mode) IsWorker() bool { return m == ModeWorker || m == ModeAll }

// -------------------------------------------------------------------------------
// Debug Configuration
//
// Author: Alex Freidah
//
// Operator-facing knobs for opt-in diagnostic features. Today this is just the
// runtime/trace.FlightRecorder (Go 1.25) that backs the
// POST /admin/api/trace/snapshot endpoint; other debug toggles can land here
// without re-shaping the root config.
// -------------------------------------------------------------------------------

package config

import "time"

// DebugConfig groups opt-in diagnostic features. All sub-blocks default to
// disabled; production deployments enable only what an incident requires.
type DebugConfig struct {
	FlightRecorder FlightRecorderConfig `yaml:"flight_recorder"`
}

// FlightRecorderConfig configures the always-on runtime/trace.FlightRecorder
// ring buffer that backs the admin trace-snapshot endpoint. Disabled by
// default — the recorder is cheap but does carry continuous overhead, so
// only flip it on where operators actually want the safety net.
// MinAge maps directly to runtime/trace.FlightRecorderConfig.MinAge and
// defaults to 30s, long enough to cover a typical incident window without
// holding excessive memory.
type FlightRecorderConfig struct {
	Enabled bool          `yaml:"enabled"` // when false, the admin snapshot endpoint returns 503
	MinAge  time.Duration `yaml:"min_age"` // soft lower bound on the trace window age
}

// setDefaultsAndValidate populates defaults and returns any validation
// errors. A disabled recorder skips validation entirely.
func (fr *FlightRecorderConfig) setDefaultsAndValidate() []error {
	if !fr.Enabled {
		return nil
	}
	if fr.MinAge <= 0 {
		fr.MinAge = 30 * time.Second
	}
	return nil
}

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
type FlightRecorderConfig struct {
	// Enabled starts the FlightRecorder at boot. When false the admin
	// snapshot endpoint returns 503.
	Enabled bool `yaml:"enabled"`

	// MinAge is the soft lower bound on the trace window age, mapped
	// directly to runtime/trace.FlightRecorderConfig.MinAge. Defaults to
	// 30s — long enough to cover a typical incident window without
	// excessive memory.
	MinAge time.Duration `yaml:"min_age"`
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

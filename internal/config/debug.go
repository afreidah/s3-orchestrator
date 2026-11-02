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

import (
	"cmp"
	"time"
)

// DebugConfig groups opt-in diagnostic features. All sub-blocks default to
// disabled; production deployments enable only what an incident requires.
type DebugConfig struct {
	FlightRecorder FlightRecorderConfig `yaml:"flight_recorder"`
}

// FlightRecorderConfig configures the always-on runtime/trace.FlightRecorder
// ring buffer that backs the admin trace-snapshot endpoint. Disabled by
// default because the recorder carries continuous overhead. MinAge maps to
// runtime/trace.FlightRecorderConfig.MinAge and defaults to 30s.
type FlightRecorderConfig struct {
	Enabled bool          `yaml:"enabled"` // when false, the admin snapshot endpoint returns 503
	MinAge  time.Duration `yaml:"min_age"` // soft lower bound on the trace window age
}

// setDefaultsAndValidate gives the recorder its default window age and refuses
// a negative one.
func (fr *FlightRecorderConfig) setDefaultsAndValidate() []error {
	fr.MinAge = cmp.Or(fr.MinAge, 30*time.Second)
	return failed(positive("debug.flight_recorder.min_age", fr.MinAge))
}

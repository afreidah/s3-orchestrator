// -------------------------------------------------------------------------------
// Flight Recorder Config Tests
//
// Author: Alex Freidah
//
// Covers the validation and defaulting of the flight recorder block. A
// disabled recorder is not validated at all, and an enabled one gets a
// min_age default rather than a zero that would trip on every trace.
// -------------------------------------------------------------------------------

package config

import (
	"testing"
	"time"
)

// TestFlightRecorder_Disabled_LeftAsWritten pins that a disabled block gets
// no defaults, whatever its fields hold, since nothing reads it.
func TestFlightRecorder_Disabled_LeftAsWritten(t *testing.T) {
	t.Parallel()
	fr := FlightRecorderConfig{Enabled: false, MinAge: -5 * time.Second}
	fr.setDefaults()
	if fr.MinAge != -5*time.Second {
		t.Errorf("MinAge = %v, want the written -5s left alone", fr.MinAge)
	}
}

// TestFlightRecorder_Enabled_DefaultsMinAge verifies the 30s default is
// applied when MinAge is zero.
func TestFlightRecorder_Enabled_DefaultsMinAge(t *testing.T) {
	t.Parallel()
	fr := FlightRecorderConfig{Enabled: true}
	fr.setDefaults()
	if fr.MinAge != 30*time.Second {
		t.Errorf("MinAge = %v, want 30s", fr.MinAge)
	}
}

// TestFlightRecorder_Enabled_KeepsCustomMinAge verifies a caller-supplied
// MinAge is not overwritten.
func TestFlightRecorder_Enabled_KeepsCustomMinAge(t *testing.T) {
	t.Parallel()
	fr := FlightRecorderConfig{Enabled: true, MinAge: 90 * time.Second}
	fr.setDefaults()
	if fr.MinAge != 90*time.Second {
		t.Errorf("MinAge = %v, want 90s (caller-supplied)", fr.MinAge)
	}
}

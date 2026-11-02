// -------------------------------------------------------------------------------
// Flight Recorder Config Tests
//
// Author: Alex Freidah
//
// Covers the defaulting and validation of the flight recorder block: a zero
// min_age takes the default rather than tripping on every trace, a set one is
// kept, and a negative one is refused.
// -------------------------------------------------------------------------------

package config

import (
	"errors"
	"testing"
	"time"
)

// TestFlightRecorder_DefaultsMinAge verifies the 30s default is applied when
// MinAge is zero.
func TestFlightRecorder_DefaultsMinAge(t *testing.T) {
	t.Parallel()
	fr := FlightRecorderConfig{Enabled: true}
	if errs := fr.setDefaultsAndValidate(); len(errs) != 0 || fr.MinAge != 30*time.Second {
		t.Errorf("MinAge = %v, errs %v; want 30s and none", fr.MinAge, errs)
	}
}

// TestFlightRecorder_KeepsCustomMinAge verifies a caller-supplied MinAge is
// not overwritten.
func TestFlightRecorder_KeepsCustomMinAge(t *testing.T) {
	t.Parallel()
	fr := FlightRecorderConfig{Enabled: true, MinAge: 90 * time.Second}
	if errs := fr.setDefaultsAndValidate(); len(errs) != 0 || fr.MinAge != 90*time.Second {
		t.Errorf("MinAge = %v, errs %v; want 90s (caller-supplied) and none", fr.MinAge, errs)
	}
}

// TestFlightRecorder_RefusesNegativeMinAge verifies a negative window is
// refused rather than rewritten.
func TestFlightRecorder_RefusesNegativeMinAge(t *testing.T) {
	t.Parallel()
	fr := FlightRecorderConfig{MinAge: -5 * time.Second}
	if errs := fr.setDefaultsAndValidate(); len(errs) != 1 || !errors.Is(errs[0], ErrNotPositive) {
		t.Errorf("errs = %v, want one ErrNotPositive", errs)
	}
}

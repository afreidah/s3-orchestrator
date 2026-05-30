package config

import (
	"testing"
	"time"
)

// TestFlightRecorder_Disabled_NoValidation pins that a disabled block
// skips validation entirely (even with absurd field values).
func TestFlightRecorder_Disabled_NoValidation(t *testing.T) {
	t.Parallel()
	fr := FlightRecorderConfig{Enabled: false, MinAge: -5 * time.Second}
	if errs := fr.setDefaultsAndValidate(); errs != nil {
		t.Errorf("disabled config should skip validation, got %v", errs)
	}
}

// TestFlightRecorder_Enabled_DefaultsMinAge verifies the 30s default is
// applied when MinAge is zero.
func TestFlightRecorder_Enabled_DefaultsMinAge(t *testing.T) {
	t.Parallel()
	fr := FlightRecorderConfig{Enabled: true}
	if errs := fr.setDefaultsAndValidate(); errs != nil {
		t.Fatalf("unexpected errs: %v", errs)
	}
	if fr.MinAge != 30*time.Second {
		t.Errorf("MinAge = %v, want 30s", fr.MinAge)
	}
}

// TestFlightRecorder_Enabled_KeepsCustomMinAge verifies a caller-supplied
// MinAge is not overwritten.
func TestFlightRecorder_Enabled_KeepsCustomMinAge(t *testing.T) {
	t.Parallel()
	fr := FlightRecorderConfig{Enabled: true, MinAge: 90 * time.Second}
	if errs := fr.setDefaultsAndValidate(); errs != nil {
		t.Fatalf("unexpected errs: %v", errs)
	}
	if fr.MinAge != 90*time.Second {
		t.Errorf("MinAge = %v, want 90s (caller-supplied)", fr.MinAge)
	}
}

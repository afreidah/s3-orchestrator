// -------------------------------------------------------------------------------
// Integrity Configuration Tests
//
// Author: Alex Freidah
//
// Covers the verify-on-replicate gate and the scrubber defaults. The gate table
// is the load-bearing one: it pins that enabling integrity does not by itself
// turn on the one integrity check that costs a full extra read of every replica.
// -------------------------------------------------------------------------------

package config

import (
	"testing"
	"time"
)

// TestIntegrityConfig_ShouldVerifyOnReplicate pins the gate to both flags.
//
// The "enabled alone" row is the point of the table. Verification doubles the
// egress a replica costs, so it stays opt-in like verify_on_read rather than
// arriving as a side effect of asking for content hashes.
func TestIntegrityConfig_ShouldVerifyOnReplicate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  IntegrityConfig
		want bool
	}{
		{"zero value", IntegrityConfig{}, false},
		{"enabled alone does not verify replicas", IntegrityConfig{Enabled: true}, false},
		{"asked for but integrity off", IntegrityConfig{VerifyOnReplicate: true}, false},
		{"both set", IntegrityConfig{Enabled: true, VerifyOnReplicate: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.ShouldVerifyOnReplicate(); got != tc.want {
				t.Errorf("ShouldVerifyOnReplicate() = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestIntegrityConfig_DisabledSkipsValidation verifies a disabled block is left
// alone, so a half-filled section cannot fail startup or acquire defaults it
// will never use.
func TestIntegrityConfig_DisabledSkipsValidation(t *testing.T) {
	t.Parallel()
	ic := IntegrityConfig{ScrubberInterval: -1}
	if errs := ic.setDefaultsAndValidate(); len(errs) != 0 {
		t.Fatalf("disabled block reported %d errors, want 0", len(errs))
	}
	if ic.ScrubberBatchSize != 0 {
		t.Errorf("ScrubberBatchSize = %d, want 0 (untouched while disabled)", ic.ScrubberBatchSize)
	}
}

// TestIntegrityConfig_EnabledDefaultsAndValidation covers the two things
// setDefaultsAndValidate does once the block is on.
func TestIntegrityConfig_EnabledDefaultsAndValidation(t *testing.T) {
	t.Parallel()

	t.Run("batch size defaults", func(t *testing.T) {
		t.Parallel()
		ic := IntegrityConfig{Enabled: true}
		if errs := ic.setDefaultsAndValidate(); len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if ic.ScrubberBatchSize != 100 {
			t.Errorf("ScrubberBatchSize = %d, want 100", ic.ScrubberBatchSize)
		}
	})

	t.Run("explicit batch size is kept", func(t *testing.T) {
		t.Parallel()
		ic := IntegrityConfig{Enabled: true, ScrubberBatchSize: 25}
		if errs := ic.setDefaultsAndValidate(); len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if ic.ScrubberBatchSize != 25 {
			t.Errorf("ScrubberBatchSize = %d, want 25", ic.ScrubberBatchSize)
		}
	})

	t.Run("negative interval is rejected", func(t *testing.T) {
		t.Parallel()
		ic := IntegrityConfig{Enabled: true, ScrubberInterval: -time.Second}
		if errs := ic.setDefaultsAndValidate(); len(errs) != 1 {
			t.Fatalf("got %d errors, want 1", len(errs))
		}
	})
}

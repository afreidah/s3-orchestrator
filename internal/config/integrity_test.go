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
	"crypto/sha256"
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

// TestIntegrityConfig_NilSafeGates verifies every gate answers for a nil,
// disabled and enabled config, so callers loading an unset config need no nil
// check.
func TestIntegrityConfig_NilSafeGates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                         string
		cfg                          *IntegrityConfig
		enabled, onRead, onReplicate bool
	}{
		{"nil", nil, false, false, false},
		{"disabled with flags set", &IntegrityConfig{VerifyOnRead: true, VerifyOnReplicate: true}, false, false, false},
		{"enabled alone", &IntegrityConfig{Enabled: true}, true, false, false},
		{"enabled with flags set", &IntegrityConfig{Enabled: true, VerifyOnRead: true, VerifyOnReplicate: true}, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.IsEnabled(); got != tc.enabled {
				t.Errorf("IsEnabled() = %t, want %t", got, tc.enabled)
			}
			if got := tc.cfg.ShouldVerifyOnRead(); got != tc.onRead {
				t.Errorf("ShouldVerifyOnRead() = %t, want %t", got, tc.onRead)
			}
			if got := tc.cfg.ShouldVerifyOnReplicate(); got != tc.onReplicate {
				t.Errorf("ShouldVerifyOnReplicate() = %t, want %t", got, tc.onReplicate)
			}
			if got := tc.cfg.Hasher() != nil; got != tc.enabled {
				t.Errorf("Hasher() non-nil = %t, want %t", got, tc.enabled)
			}
		})
	}
}

// TestIntegrityConfig_HasherIsSHA256 verifies an enabled config hashes with
// SHA-256 and hands out a fresh hasher on every call.
func TestIntegrityConfig_HasherIsSHA256(t *testing.T) {
	t.Parallel()
	ic := &IntegrityConfig{Enabled: true}
	h := ic.Hasher()
	if h.Size() != sha256.Size {
		t.Errorf("Hasher().Size() = %d, want %d", h.Size(), sha256.Size)
	}
	if h == ic.Hasher() {
		t.Error("Hasher() returned the same hasher twice")
	}
}

// TestIntegrityConfig_ScrubbedBeforeNilConfig verifies a nil config uses the
// default floor instead of panicking.
func TestIntegrityConfig_ScrubbedBeforeNilConfig(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)
	var ic *IntegrityConfig
	if got, want := ic.ScrubbedBefore(now), now.Add(-DefaultScrubberMinAge); !got.Equal(want) {
		t.Errorf("ScrubbedBefore = %v, want %v", got, want)
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

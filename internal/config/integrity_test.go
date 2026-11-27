// -------------------------------------------------------------------------------
// Integrity Configuration Tests
//
// Author: Alex Freidah
//
// Covers the verification gates and the scrubber defaults. The gate table is
// the load-bearing one: it pins that enabling integrity does not by itself turn
// on the checks that cost an extra read, and that the checks do not need it.
// -------------------------------------------------------------------------------

package config

import (
	"testing"
	"time"
)

// TestIntegrityConfig_VerifyGates pins each verification gate to its own flag.
//
// The "enabled alone" row is the point of the table. Verifying a replica
// doubles the egress it costs, so it stays opt-in rather than arriving as a
// side effect of asking for content hashes. A nil config, an integrity section
// never loaded, verifies nothing.
func TestIntegrityConfig_VerifyGates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                string
		cfg                 *IntegrityConfig
		onRead, onReplicate bool
	}{
		{"nil", nil, false, false},
		{"zero value", &IntegrityConfig{}, false, false},
		{"enabled alone verifies nothing", &IntegrityConfig{Enabled: true}, false, false},
		{"flags without enabled", &IntegrityConfig{VerifyOnRead: true, VerifyOnReplicate: true}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.ShouldVerifyOnRead(); got != tc.onRead {
				t.Errorf("ShouldVerifyOnRead() = %t, want %t", got, tc.onRead)
			}
			if got := tc.cfg.ShouldVerifyOnReplicate(); got != tc.onReplicate {
				t.Errorf("ShouldVerifyOnReplicate() = %t, want %t", got, tc.onReplicate)
			}
		})
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

// TestIntegrityConfig_ScrubReserve pins the reserve to the configured share of
// the backend's egress limit. An unset share or a nil config takes the default,
// an explicit zero reserves nothing, and a backend with no limit has no reserve.
func TestIntegrityConfig_ScrubReserve(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		ic    *IntegrityConfig
		limit int64
		want  int64
	}{
		{"nil config", nil, 1000, 800},
		{"unset", &IntegrityConfig{}, 1000, 800},
		{"configured", &IntegrityConfig{ScrubberEgressReserve: new(0.5)}, 1000, 500},
		{"zero", &IntegrityConfig{ScrubberEgressReserve: new(0.0)}, 1000, 0},
		{"no egress limit", &IntegrityConfig{}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.ic.ScrubReserve(tc.limit); got != tc.want {
				t.Errorf("ScrubReserve(%d) = %d, want %d", tc.limit, got, tc.want)
			}
		})
	}
}

// TestIntegrityConfig_ScrubberEgressReserveValidation checks an enabled block
// is given the default share and a share outside 0 to 1 is rejected.
func TestIntegrityConfig_ScrubberEgressReserveValidation(t *testing.T) {
	t.Parallel()
	ic := IntegrityConfig{Enabled: true}
	if errs := ic.setDefaultsAndValidate(); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if r := ic.ScrubberEgressReserve; r == nil || *r != DefaultScrubberEgressReserve {
		t.Errorf("ScrubberEgressReserve = %v, want %v", r, DefaultScrubberEgressReserve)
	}
	for _, share := range []float64{-0.1, 1.1} {
		ic := IntegrityConfig{Enabled: true, ScrubberEgressReserve: new(share)}
		if errs := ic.setDefaultsAndValidate(); len(errs) == 0 {
			t.Errorf("scrubber_egress_reserve %v was accepted", share)
		}
	}
}

// TestIntegrityConfig_DisabledIsStillValidated verifies a disabled block is
// validated and given its defaults, since enabled only decides whether a bucket
// hashes by default and the scrubber still verifies copies that carry a hash.
func TestIntegrityConfig_DisabledIsStillValidated(t *testing.T) {
	t.Parallel()
	ic := IntegrityConfig{ScrubberInterval: -1}
	if errs := ic.setDefaultsAndValidate(); len(errs) != 1 {
		t.Fatalf("disabled block reported %v, want the negative interval", errs)
	}
	if ic.ScrubberBatchSize != 100 {
		t.Errorf("ScrubberBatchSize = %d, want the default 100", ic.ScrubberBatchSize)
	}
}

// TestIntegrityConfig_ReportsEveryError verifies an enabled block with several
// bad fields reports each of them, not only the first.
func TestIntegrityConfig_ReportsEveryError(t *testing.T) {
	t.Parallel()
	ic := IntegrityConfig{Enabled: true, ScrubberInterval: -1, ScrubberMinAge: -1, ScrubberEgressReserve: new(2.0)}
	if errs := ic.setDefaultsAndValidate(); len(errs) != 3 {
		t.Errorf("errs = %v, want 3", errs)
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

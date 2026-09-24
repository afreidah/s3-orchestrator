// -------------------------------------------------------------------------------
// Integrity Configuration
//
// Author: Alex Freidah
//
// Defines IntegrityConfig: enables SHA-256 content hashing on writes,
// optional verification on reads, and the periodic scrubber that random-
// samples stored objects to catch silent corruption. Carries the
// scrubber interval and batch size and validates them so a typo cannot
// silently disable the worker by setting the interval to zero.
// -------------------------------------------------------------------------------

package config

import (
	"fmt"
	"time"
)

// DefaultScrubberMinAge is how recently a copy must have been verified for the
// scrubber to pass over it when the integrity config does not specify a floor.
const DefaultScrubberMinAge = 24 * time.Hour

// IntegrityConfig holds settings for object integrity verification.
// When enabled, objects are checksummed on write and optionally verified
// on read and during replication.
type IntegrityConfig struct {
	Enabled           bool          `yaml:"enabled"`             // Enable integrity verification (default: false)
	VerifyOnRead      bool          `yaml:"verify_on_read"`      // Hash-check every GET response (default: false)
	VerifyOnReplicate bool          `yaml:"verify_on_replicate"` // Hash-check a new replica before recording it (default: false)
	ScrubberInterval  time.Duration `yaml:"scrubber_interval"`   // Background verification interval (0 = disabled)
	ScrubberBatchSize int           `yaml:"scrubber_batch_size"` // Objects per scrub cycle (default: 100)
	ScrubberMinAge    time.Duration `yaml:"scrubber_min_age"`    // Minimum age before a copy is re-verified (default: 24h)
}

// ScrubbedBefore returns the cutoff a scrub batch selects against: a copy last
// touched at or after it is too recently verified to be worth another read.
//
// The floor is what decouples scrub cost from the interval. Without it a
// backend holding fewer copies than the batch size is read in full on every
// pass, so egress scales with how often the scrubber runs rather than with how
// stale the data is.
//
// An unset floor resolves to the default rather than to none. Validation
// supplies the default for a parsed config, but a config built in code reaches
// the scrubber without passing through it, and no floor at all is the one
// setting an operator cannot ask for.
func (ic *IntegrityConfig) ScrubbedBefore(now time.Time) time.Time {
	minAge := ic.ScrubberMinAge
	if minAge <= 0 {
		minAge = DefaultScrubberMinAge
	}
	return now.Add(-minAge)
}

// ShouldVerifyOnReplicate reports whether a new replica must be read back and
// hash-checked before its ledger row is written.
//
// Off by default, like every other integrity check that costs a backend read:
// verifying a replica doubles the egress replication spends on it, and that is
// an operator's decision to make rather than a side effect of enabling hashing.
func (ic *IntegrityConfig) ShouldVerifyOnReplicate() bool {
	return ic.Enabled && ic.VerifyOnReplicate
}

// setDefaultsAndValidate is a no-op when integrity is disabled.
func (ic *IntegrityConfig) setDefaultsAndValidate() []error {
	if !ic.Enabled {
		return nil
	}

	if ic.ScrubberBatchSize <= 0 {
		ic.ScrubberBatchSize = 100
	}

	if ic.ScrubberMinAge == 0 {
		ic.ScrubberMinAge = DefaultScrubberMinAge
	}

	if ic.ScrubberInterval < 0 {
		return []error{fmt.Errorf("integrity.scrubber_interval must be >= 0")}
	}

	if ic.ScrubberMinAge < 0 {
		return []error{fmt.Errorf("integrity.scrubber_min_age must be >= 0")}
	}

	return nil
}

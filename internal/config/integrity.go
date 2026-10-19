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
	"crypto/sha256"
	"fmt"
	"hash"
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
// touched at or after it is too recently verified to be worth another read, so
// a backend smaller than the batch is not reread in full every pass. An unset
// floor, or a nil config, uses the default, since a config built in code skips
// validation and an unset value must not turn every pass into a full re-read.
func (ic *IntegrityConfig) ScrubbedBefore(now time.Time) time.Time {
	minAge := DefaultScrubberMinAge
	if ic != nil && ic.ScrubberMinAge > 0 {
		minAge = ic.ScrubberMinAge
	}
	return now.Add(-minAge)
}

// IsEnabled reports whether integrity verification is on. A nil config, an
// integrity section never loaded, is off, so callers need no nil check.
func (ic *IntegrityConfig) IsEnabled() bool {
	return ic != nil && ic.Enabled
}

// ShouldVerifyOnReplicate reports whether a new replica must be read back and
// hash-checked before its ledger row is written. It is off unless explicitly
// set, since verifying doubles the egress replication spends on each replica.
func (ic *IntegrityConfig) ShouldVerifyOnReplicate() bool {
	return ic.IsEnabled() && ic.VerifyOnReplicate
}

// ShouldVerifyOnRead reports whether a whole-object GET hashes the bytes it
// serves and drops a copy that does not match its stored hash.
func (ic *IntegrityConfig) ShouldVerifyOnRead() bool {
	return ic.IsEnabled() && ic.VerifyOnRead
}

// Hasher returns the hasher a content hash is computed in, by a write that
// records one and a read that checks one, or nil when integrity is off, so a
// write records no hash.
func (ic *IntegrityConfig) Hasher() hash.Hash {
	if !ic.IsEnabled() {
		return nil
	}
	return sha256.New()
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

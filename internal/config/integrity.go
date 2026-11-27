// -------------------------------------------------------------------------------
// Integrity Configuration
//
// Author: Alex Freidah
//
// Defines IntegrityConfig: whether a bucket without a stored form of its own
// hashes its objects on write, optional verification on reads, and the
// periodic scrubber that samples stored copies carrying a hash to catch silent
// corruption. Carries the scrubber interval and batch size and validates them
// so a typo cannot silently disable the worker by setting the interval to zero.
// -------------------------------------------------------------------------------

package config

import (
	"cmp"
	"time"
)

// DefaultScrubberMinAge is how recently a copy must have been verified for the
// scrubber to pass over it when the integrity config does not specify a floor.
const DefaultScrubberMinAge = 7 * 24 * time.Hour

// DefaultScrubberEgressReserve is the share of each backend's monthly egress
// limit the scrubber leaves for other reads when the config does not set one.
const DefaultScrubberEgressReserve = 0.8

// IntegrityConfig holds settings for object integrity verification. Enabled
// gives a bucket without a stored form of its own the hashed layer; a copy
// carrying a hash is verified on read, on replication and by the scrubber as
// those are configured, whichever bucket it is in.
type IntegrityConfig struct {
	Enabled               bool          `yaml:"enabled"`                 // Hash new objects by default (default: false)
	VerifyOnRead          bool          `yaml:"verify_on_read"`          // Hash-check every GET response (default: false)
	VerifyOnReplicate     bool          `yaml:"verify_on_replicate"`     // Hash-check a new replica before recording it (default: false)
	ScrubberInterval      time.Duration `yaml:"scrubber_interval"`       // Background verification interval (0 = disabled)
	ScrubberBatchSize     int           `yaml:"scrubber_batch_size"`     // Objects per scrub cycle (default: 100)
	ScrubberMinAge        time.Duration `yaml:"scrubber_min_age"`        // Minimum age before a copy is re-verified (default: 7d)
	ScrubberEgressReserve *float64      `yaml:"scrubber_egress_reserve"` // Share of egress_byte_limit the scrubber leaves for other reads (default: 0.8)
}

// ScrubReserve returns how many bytes of a backend's monthly egress limit the
// scrubber must leave unspent. A backend with no egress limit has no reserve.
// An unset share, or a nil config, uses the default, like ScrubbedBefore.
func (ic *IntegrityConfig) ScrubReserve(egressLimit int64) int64 {
	share := DefaultScrubberEgressReserve
	if ic != nil && ic.ScrubberEgressReserve != nil {
		share = *ic.ScrubberEgressReserve
	}
	return int64(share * float64(egressLimit))
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

// ShouldVerifyOnReplicate reports whether a new replica must be read back and
// hash-checked before its ledger row is written. It is off unless explicitly
// set, since verifying doubles the egress replication spends on each replica.
// A nil config, an integrity section never loaded, is off.
func (ic *IntegrityConfig) ShouldVerifyOnReplicate() bool {
	return ic != nil && ic.VerifyOnReplicate
}

// ShouldVerifyOnRead reports whether a whole-object GET hashes the bytes it
// serves and drops a copy that does not match its stored hash.
func (ic *IntegrityConfig) ShouldVerifyOnRead() bool {
	return ic != nil && ic.VerifyOnRead
}

// setDefaultsAndValidate fills the scrubber's defaults and reports every field
// out of bounds. They are checked whether or not integrity is enabled, since
// enabled only decides whether a bucket hashes by default and the scrubber
// verifies whatever copies carry a hash.
func (ic *IntegrityConfig) setDefaultsAndValidate() []error {
	ic.ScrubberBatchSize = cmp.Or(ic.ScrubberBatchSize, 100)
	ic.ScrubberMinAge = cmp.Or(ic.ScrubberMinAge, DefaultScrubberMinAge)
	if ic.ScrubberEgressReserve == nil {
		ic.ScrubberEgressReserve = new(DefaultScrubberEgressReserve)
	}
	return failed(
		nonNegative("integrity.scrubber_interval", ic.ScrubberInterval),
		positive("integrity.scrubber_batch_size", ic.ScrubberBatchSize),
		positive("integrity.scrubber_min_age", ic.ScrubberMinAge),
		between("integrity.scrubber_egress_reserve", *ic.ScrubberEgressReserve, 0, 1),
	)
}

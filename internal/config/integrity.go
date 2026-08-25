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

// IntegrityConfig holds settings for object integrity verification.
// When enabled, objects are checksummed on write and optionally verified
// on read and during replication.
type IntegrityConfig struct {
	Enabled           bool          `yaml:"enabled"`             // Enable integrity verification (default: false)
	VerifyOnRead      bool          `yaml:"verify_on_read"`      // Hash-check every GET response (default: false)
	VerifyOnReplicate bool          `yaml:"verify_on_replicate"` // Hash-check a new replica before recording it (default: false)
	ScrubberInterval  time.Duration `yaml:"scrubber_interval"`   // Background verification interval (0 = disabled)
	ScrubberBatchSize int           `yaml:"scrubber_batch_size"` // Objects per scrub cycle (default: 100)
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

	if ic.ScrubberInterval < 0 {
		return []error{fmt.Errorf("integrity.scrubber_interval must be >= 0")}
	}

	return nil
}

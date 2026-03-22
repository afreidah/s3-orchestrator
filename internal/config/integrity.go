// -------------------------------------------------------------------------------
// Integrity Configuration
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package config

import "time"

// IntegrityConfig holds settings for object integrity verification.
// When enabled, objects are checksummed on write and optionally verified
// on read and during replication.
type IntegrityConfig struct {
	Enabled           bool          `yaml:"enabled"`              // Enable integrity verification (default: false)
	VerifyOnRead      bool          `yaml:"verify_on_read"`       // Hash-check every GET response (default: false)
	VerifyOnReplicate *bool         `yaml:"verify_on_replicate"`  // Hash-check before recording a replica (default: true when enabled)
	ScrubberInterval  time.Duration `yaml:"scrubber_interval"`    // Background verification interval (0 = disabled)
	ScrubberBatchSize int           `yaml:"scrubber_batch_size"`  // Objects per scrub cycle (default: 100)
}

// ShouldVerifyOnReplicate returns whether replication copies should be
// hash-verified. Defaults to true when integrity is enabled.
func (ic *IntegrityConfig) ShouldVerifyOnReplicate() bool {
	if !ic.Enabled {
		return false
	}
	if ic.VerifyOnReplicate == nil {
		return true // default when enabled
	}
	return *ic.VerifyOnReplicate
}

// setDefaultsAndValidate is a no-op when integrity is disabled.
// When enabled, VerifyOnReplicate defaults to true unless explicitly
// set to false in the YAML (Go's zero-value for bool is false, so we
// use a pointer internally during parsing — but since the config is
// simple YAML, we just default it here and document the behavior).
func (ic *IntegrityConfig) setDefaultsAndValidate() []string {
	if !ic.Enabled {
		return nil
	}

	if ic.ScrubberBatchSize <= 0 {
		ic.ScrubberBatchSize = 100
	}

	if ic.ScrubberInterval < 0 {
		return []string{"integrity.scrubber_interval must be >= 0"}
	}

	return nil
}

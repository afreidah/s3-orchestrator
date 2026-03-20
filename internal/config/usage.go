// -------------------------------------------------------------------------------
// Usage Flush Configuration
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package config

import "time"

// UsageFlushConfig holds settings for the periodic usage counter flush to the
// database. When adaptive flushing is enabled, the flush interval shortens
// automatically when any backend approaches a usage limit.
type UsageFlushConfig struct {
	Interval          time.Duration `yaml:"interval"`           // Base flush interval (default: 30s)
	AdaptiveEnabled   bool          `yaml:"adaptive_enabled"`   // Shorten interval near limits
	AdaptiveThreshold float64       `yaml:"adaptive_threshold"` // Ratio to trigger fast flush (default: 0.8)
	FastInterval      time.Duration `yaml:"fast_interval"`      // Interval when near limits (default: 5s)
}

func (u *UsageFlushConfig) setDefaultsAndValidate() []string {
	var errs []string

	if u.Interval == 0 {
		u.Interval = 30 * time.Second
	}
	if u.AdaptiveThreshold == 0 {
		u.AdaptiveThreshold = 0.8
	}
	if u.FastInterval == 0 {
		u.FastInterval = 5 * time.Second
	}

	if u.Interval <= 0 {
		errs = append(errs, "usage_flush.interval must be positive")
	}
	if u.AdaptiveThreshold <= 0 || u.AdaptiveThreshold >= 1 {
		errs = append(errs, "usage_flush.adaptive_threshold must be between 0 and 1 (exclusive)")
	}
	if u.FastInterval <= 0 {
		errs = append(errs, "usage_flush.fast_interval must be positive")
	}
	if u.FastInterval >= u.Interval {
		errs = append(errs, "usage_flush.fast_interval must be less than usage_flush.interval")
	}

	return errs
}

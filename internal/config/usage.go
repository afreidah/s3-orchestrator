// -------------------------------------------------------------------------------
// Usage Flush Configuration
//
// Author: Alex Freidah
//
// Defines UsageFlushConfig - tuning for the periodic flush that drains
// in-memory or Redis usage deltas into backend_usage. Carries the base
// interval, the adaptive interval used when any backend is near a
// monthly limit, and the watchdog timeout that resets a stalled
// half-open advisory lock so the flusher cannot get pinned by a hung
// instance.
// -------------------------------------------------------------------------------

package config

import (
	"fmt"
	"time"
)

// UsageFlushConfig holds settings for the periodic usage counter flush to the
// database. When adaptive flushing is enabled, the flush interval shortens
// automatically when any backend approaches a usage limit.
type UsageFlushConfig struct {
	Interval          time.Duration `yaml:"interval"`           // Base flush interval (default: 30s)
	AdaptiveEnabled   bool          `yaml:"adaptive_enabled"`   // Shorten interval near limits
	AdaptiveThreshold float64       `yaml:"adaptive_threshold"` // Ratio to trigger fast flush (default: 0.8)
	FastInterval      time.Duration `yaml:"fast_interval"`      // Interval when near limits (default: 5s)
}

// setDefaultsAndValidate sets defaults and validate.
func (u *UsageFlushConfig) setDefaultsAndValidate() []error {
	var errs []error

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
		errs = append(errs, fmt.Errorf("usage_flush.interval must be positive"))
	}
	if u.AdaptiveThreshold <= 0 || u.AdaptiveThreshold >= 1 {
		errs = append(errs, ErrUsageFlushThresholdRange)
	}
	if u.FastInterval <= 0 {
		errs = append(errs, fmt.Errorf("usage_flush.fast_interval must be positive"))
	}
	if u.FastInterval >= u.Interval {
		errs = append(errs, ErrUsageFlushFastInterval)
	}

	return errs
}

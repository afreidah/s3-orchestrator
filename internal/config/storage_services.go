// -------------------------------------------------------------------------------
// Storage Service Configuration  -  Rebalance, Replication, Cleanup, Lifecycle, Reconcile
//
// Author: Alex Freidah
//
// Defines the per-background-service config blocks: RebalanceConfig
// (enable + threshold + interval), ReplicationConfig (factor and
// concurrency), CleanupQueueConfig (concurrency), LifecycleRules
// (prefix + expiration_days array), ReconcileConfig (interval), and
// the PendingReaperConfig that controls the PUT-before-COMMIT intent
// reaper. All blocks are hot-reloadable via SIGHUP; validators ensure
// every interval is positive and every factor stays within sane
// bounds so a misconfigured reload cannot disable a worker.
// -------------------------------------------------------------------------------

package config

import (
	"fmt"
	"time"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// RebalanceConfig holds settings for the periodic backend rebalancer.
// Disabled by default to avoid unexpected API calls and egress charges.
type RebalanceConfig struct {
	Enabled     bool          `yaml:"enabled"`
	Strategy    string        `yaml:"strategy"`    // "pack" or "spread"
	Interval    time.Duration `yaml:"interval"`
	BatchSize   int           `yaml:"batch_size"`
	Threshold   float64       `yaml:"threshold"`   // min utilization spread to trigger
	Concurrency int           `yaml:"concurrency"` // parallel moves (default: 5)
}

// ReplicationConfig holds settings for the background replication worker.
// When factor is 1, replication is disabled and behavior is identical to
// the single-copy default.
type ReplicationConfig struct {
	Factor             int           `yaml:"factor"`
	WorkerInterval     time.Duration `yaml:"worker_interval"`
	BatchSize          int           `yaml:"batch_size"`
	Concurrency        int           `yaml:"concurrency"`          // Parallel object replications (default: 5)
	UnhealthyThreshold time.Duration `yaml:"unhealthy_threshold"`  // Grace period before replacing copies on circuit-broken backends (default: 10m)
}

// CleanupQueueConfig holds settings for the background orphan cleanup worker
// and multipart upload housekeeping.
//
// ClaimGracePeriod controls how long a per-row claim stamp held by an
// instance remains exclusive before another instance is allowed to reclaim
// the row. A short value lets a crashed worker's rows recover quickly at
// the cost of a higher chance of duplicate processing if a real worker is
// merely slow; a long value is the inverse trade-off. The 5-minute default
// covers the realistic worst case for a single backend DELETE plus its
// retry budget within one tick. Hot-reloadable.
type CleanupQueueConfig struct {
	Concurrency           int           `yaml:"concurrency"`             // Parallel cleanup deletions (default: 10)
	MultipartStaleTimeout time.Duration `yaml:"multipart_stale_timeout"` // Abandon multipart uploads older than this (default: 24h)
	ClaimGracePeriod      time.Duration `yaml:"claim_grace_period"`      // Reclaim stale per-row claims older than this (default: 5m)
}

// WritePathConfig gates write-path correctness features. The pending-row
// pattern (PUT-before-COMMIT intent tracking) is on by default; operators
// can disable it to fall back to the legacy delete-on-record-failure path,
// which trades data-loss safety for one fewer round-trip per PUT.
type WritePathConfig struct {
	PendingPattern PendingPatternConfig `yaml:"pending_pattern"`
}

// PendingPatternConfig configures the pending-row pattern used by the
// write path to avoid losing the prior copy of an overwritten key when
// the metadata commit fails.
type PendingPatternConfig struct {
	Enabled    *bool         `yaml:"enabled"`     // Default: true. Set to false to disable.
	ReaperTick time.Duration `yaml:"reaper_tick"` // How often the reaper resolves abandoned intents (default: 1m)
	MinAge     time.Duration `yaml:"min_age"`     // Don't reap intents younger than this  -  guards in-flight PUTs (default: 5m)
	BatchSize  int           `yaml:"batch_size"`  // Max intents resolved per tick (default: 50)
}

// IsEnabled returns true unless the operator has explicitly disabled the
// pending pattern. The pointer-typed Enabled field lets the YAML loader
// distinguish "absent" (default true) from "explicitly false".
func (p *PendingPatternConfig) IsEnabled() bool {
	return p.Enabled == nil || *p.Enabled
}

// ReconcileConfig controls the background orphan reconciler that periodically
// scans backends and imports untracked objects into the metadata database.
// Disabled by default.
type ReconcileConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"` // How often to run (default: 24h)
}

// LifecycleConfig holds rules for automatic object expiration. Objects matching
// a rule's prefix that are older than expiration_days are deleted by a background
// worker. Empty rules list disables lifecycle processing.
type LifecycleConfig struct {
	Rules     []LifecycleRule `yaml:"rules"`
	BatchSize int             `yaml:"batch_size"` // objects per DB query (default 100)
}

// LifecycleRule defines a single object expiration rule.
type LifecycleRule struct {
	Prefix         string `yaml:"prefix"`
	ExpirationDays int    `yaml:"expiration_days"`
}

// -------------------------------------------------------------------------
// VALIDATION
// -------------------------------------------------------------------------

// setDefaultsAndValidate sets defaults and validate.
func (c *LifecycleConfig) setDefaultsAndValidate() []error {
	if len(c.Rules) == 0 {
		return nil
	}

	var errs []error
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	for i, rule := range c.Rules {
		if rule.Prefix == "" {
			errs = append(errs, fmt.Errorf("lifecycle.rules[%d]: %w", i, ErrLifecyclePrefixRequired))
		}
		if rule.ExpirationDays <= 0 {
			errs = append(errs, fmt.Errorf("lifecycle.rules[%d]: %w", i, ErrInvalidExpiration))
		}
	}
	return errs
}

// setDefaultsAndValidate sets defaults and validate.
func (r *RebalanceConfig) setDefaultsAndValidate() []error {
	if !r.Enabled {
		return nil
	}

	var errs []error

	r.Strategy = defaulted(r.Strategy, "pack")
	r.Interval = defaulted(r.Interval, 6*time.Hour)
	r.BatchSize = defaulted(r.BatchSize, 100)
	r.Threshold = defaulted(r.Threshold, 0.1)
	r.Concurrency = defaulted(r.Concurrency, 5)

	if r.Strategy != "pack" && r.Strategy != "spread" {
		errs = append(errs, ErrInvalidRebalanceStrategy)
	}
	if r.Interval <= 0 {
		errs = append(errs, ErrRebalanceIntervalNotPos)
	}
	if r.BatchSize <= 0 {
		errs = append(errs, ErrRebalanceBatchNotPos)
	}
	if r.Threshold < 0 || r.Threshold > 1 {
		errs = append(errs, ErrRebalanceThresholdRange)
	}
	if r.Concurrency <= 0 {
		errs = append(errs, ErrRebalanceConcurrencyNotPos)
	}

	return errs
}

// setDefaultsAndValidate sets defaults and validate.
func (r *ReplicationConfig) setDefaultsAndValidate(backendCount int) []error {
	var errs []error
	if r.Factor == 0 {
		r.Factor = 1
	}
	if r.Factor < 1 {
		errs = append(errs, ErrReplicationFactorMin)
	}
	if r.Factor > 1 {
		r.applyReplicationDefaults()
		errs = append(errs, r.validateReplicationLimits(backendCount)...)
	}
	return errs
}

// applyReplicationDefaults fills zero-valued replication knobs with
// production defaults. Only runs when replication is actually enabled
// (Factor > 1); leaves everything at zero otherwise so operators don't
// see "configured" values when they never turned replication on.
func (r *ReplicationConfig) applyReplicationDefaults() {
	if r.WorkerInterval == 0 {
		r.WorkerInterval = 5 * time.Minute
	}
	if r.BatchSize == 0 {
		r.BatchSize = 50
	}
	if r.UnhealthyThreshold == 0 {
		r.UnhealthyThreshold = 10 * time.Minute
	}
	if r.Concurrency <= 0 {
		r.Concurrency = 5
	}
}

// validateReplicationLimits enforces the non-negative + backend-count
// invariants that only apply when Factor > 1.
func (r *ReplicationConfig) validateReplicationLimits(backendCount int) []error {
	var errs []error
	if r.Factor > backendCount {
		errs = append(errs, fmt.Errorf("%w: factor=%d backends=%d",
			ErrReplicationFactorTooLarge, r.Factor, backendCount))
	}
	if r.WorkerInterval <= 0 {
		errs = append(errs, ErrReplicationIntervalNotPos)
	}
	if r.BatchSize <= 0 {
		errs = append(errs, ErrReplicationBatchNotPos)
	}
	return errs
}

// validateLifecycleRules enforces that every configured rule has a
// non-empty prefix and a positive expiration_days. Returns the set of
// problems so a misconfigured rule cannot silently disable lifecycle
// expiration.
func validateLifecycleRules(rules []LifecycleRule) []error {
	var errs []error

	prefixes := make(map[string]bool)
	for i := range rules {
		r := &rules[i]
		prefix := fmt.Sprintf("lifecycle.rules[%d]", i)

		if r.Prefix == "" {
			errs = append(errs, fmt.Errorf("%s: %w", prefix, ErrLifecyclePrefixRequired))
		}
		if r.ExpirationDays <= 0 {
			errs = append(errs, fmt.Errorf("%s: %w", prefix, ErrInvalidExpiration))
		}
		if prefixes[r.Prefix] {
			errs = append(errs, fmt.Errorf("%s: %w: %q", prefix, ErrDuplicatePrefix, r.Prefix))
		}
		prefixes[r.Prefix] = true
	}

	return errs
}

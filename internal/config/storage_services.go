// -------------------------------------------------------------------------------
// Storage Service Configuration — Rebalance, Replication, Cleanup, Lifecycle, Reconcile
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package config

import (
	"fmt"
	"time"
)

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
type CleanupQueueConfig struct {
	Concurrency            int           `yaml:"concurrency"`              // Parallel cleanup deletions (default: 10)
	MultipartStaleTimeout  time.Duration `yaml:"multipart_stale_timeout"`  // Abandon multipart uploads older than this (default: 24h)
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

func (r *RebalanceConfig) setDefaultsAndValidate() []error {
	if !r.Enabled {
		return nil
	}

	var errs []error

	if r.Strategy == "" {
		r.Strategy = "pack"
	}
	if r.Interval == 0 {
		r.Interval = 6 * time.Hour
	}
	if r.BatchSize == 0 {
		r.BatchSize = 100
	}
	if r.Threshold == 0 {
		r.Threshold = 0.1
	}
	if r.Concurrency == 0 {
		r.Concurrency = 5
	}

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

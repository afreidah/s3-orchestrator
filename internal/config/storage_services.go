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

// CleanupQueueConfig holds settings for the background orphan cleanup worker.
type CleanupQueueConfig struct {
	Concurrency int `yaml:"concurrency"` // Parallel cleanup deletions (default: 10)
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

func (c *LifecycleConfig) setDefaultsAndValidate() []string {
	if len(c.Rules) == 0 {
		return nil
	}

	var errs []string
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	for i, rule := range c.Rules {
		if rule.Prefix == "" {
			errs = append(errs, fmt.Sprintf("lifecycle.rules[%d]: prefix must not be empty (would match all objects)", i))
		}
		if rule.ExpirationDays <= 0 {
			errs = append(errs, fmt.Sprintf("lifecycle.rules[%d]: expiration_days must be positive", i))
		}
	}
	return errs
}

func (r *RebalanceConfig) setDefaultsAndValidate() []string {
	if !r.Enabled {
		return nil
	}

	var errs []string

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
		errs = append(errs, "rebalance.strategy must be 'pack' or 'spread'")
	}
	if r.Interval <= 0 {
		errs = append(errs, "rebalance.interval must be positive")
	}
	if r.BatchSize <= 0 {
		errs = append(errs, "rebalance.batch_size must be positive")
	}
	if r.Threshold < 0 || r.Threshold > 1 {
		errs = append(errs, "rebalance.threshold must be between 0 and 1")
	}
	if r.Concurrency <= 0 {
		errs = append(errs, "rebalance.concurrency must be positive")
	}

	return errs
}

func (r *ReplicationConfig) setDefaultsAndValidate(backendCount int) []string {
	var errs []string

	if r.Factor == 0 {
		r.Factor = 1
	}
	if r.Factor < 1 {
		errs = append(errs, "replication.factor must be at least 1")
	}

	if r.Factor > 1 {
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

		if r.Factor > backendCount {
			errs = append(errs, fmt.Sprintf(
				"replication.factor (%d) cannot exceed number of backends (%d)",
				r.Factor, backendCount))
		}
		if r.WorkerInterval <= 0 {
			errs = append(errs, "replication.worker_interval must be positive")
		}
		if r.BatchSize <= 0 {
			errs = append(errs, "replication.batch_size must be positive")
		}
	}

	return errs
}

func validateLifecycleRules(rules []LifecycleRule) []string {
	var errs []string

	prefixes := make(map[string]bool)
	for i := range rules {
		r := &rules[i]
		prefix := fmt.Sprintf("lifecycle.rules[%d]", i)

		if r.Prefix == "" {
			errs = append(errs, fmt.Sprintf("%s: prefix is required", prefix))
		}
		if r.ExpirationDays <= 0 {
			errs = append(errs, fmt.Sprintf("%s: expiration_days must be positive", prefix))
		}
		if prefixes[r.Prefix] {
			errs = append(errs, fmt.Sprintf("%s: duplicate prefix '%s'", prefix, r.Prefix))
		}
		prefixes[r.Prefix] = true
	}

	return errs
}

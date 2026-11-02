// -------------------------------------------------------------------------------
// Storage Service Configuration  -  Rebalance, Replication, Cleanup, Lifecycle, Reconcile
//
// Author: Alex Freidah
//
// Defines the per-background-service config blocks: RebalanceConfig
// (enable + threshold + interval), ReplicationConfig (factor and
// concurrency), CleanupQueueConfig (concurrency), LifecycleRules
// (prefix + expiration_days array), ReconcileConfig (interval), and
// the PendingPatternConfig that controls the PUT-before-COMMIT intent
// reaper. All blocks are hot-reloadable via SIGHUP; validators ensure
// every interval is positive and every factor stays within sane
// bounds so a misconfigured reload cannot disable a worker.
// -------------------------------------------------------------------------------

package config

import (
	"cmp"
	"fmt"
	"sort"
	"strings"
	"time"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// RebalanceConfig holds settings for the periodic backend rebalancer.
// Disabled by default to avoid unexpected API calls and egress charges.
type RebalanceConfig struct {
	Enabled     bool            `yaml:"enabled"`
	Strategy    RoutingStrategy `yaml:"strategy"` // "pack" (default) or "spread"
	Interval    time.Duration   `yaml:"interval"`
	BatchSize   int             `yaml:"batch_size"`
	Threshold   float64         `yaml:"threshold"`   // min utilization spread to trigger
	Concurrency int             `yaml:"concurrency"` // parallel moves (default: 5)
}

// ReplicationConfig holds settings for the background replication worker.
// When factor is 1, replication is disabled and behavior is identical to
// the single-copy default.
type ReplicationConfig struct {
	Factor             int           `yaml:"factor"`
	WorkerInterval     time.Duration `yaml:"worker_interval"`
	BatchSize          int           `yaml:"batch_size"`
	Concurrency        int           `yaml:"concurrency"`         // Parallel object replications (default: 5)
	UnhealthyThreshold time.Duration `yaml:"unhealthy_threshold"` // Grace period before replacing copies on backends refusing reads (default: 10m)
}

// CleanupQueueConfig holds settings for the background orphan cleanup worker
// and multipart upload housekeeping.
//
// ClaimGracePeriod is how long an instance's per-row claim stays exclusive
// before another instance may reclaim the row. Shorter recovers a crashed
// worker's rows sooner but risks duplicate processing behind a slow one. The
// 5-minute default covers one backend DELETE plus its retries. Hot-reloadable.
type CleanupQueueConfig struct {
	Concurrency           int           `yaml:"concurrency"`             // Parallel cleanup deletions (default: 10)
	MultipartStaleTimeout time.Duration `yaml:"multipart_stale_timeout"` // Abandon multipart uploads older than this (default: 24h)
	ClaimGracePeriod      time.Duration `yaml:"claim_grace_period"`      // Reclaim stale per-row claims older than this (default: 5m)
}

// WritePathConfig tunes the write path: the reaper behind the pending-row
// pattern (PUT-before-COMMIT intent tracking, which every write uses),
// multipart limits, and how many copies a write places in parallel.
type WritePathConfig struct {
	PendingPattern PendingPatternConfig `yaml:"pending_pattern"`
	Multipart      MultipartConfig      `yaml:"multipart"`
	ParallelCopies ParallelCopiesConfig `yaml:"parallel_copies"`
}

// ParallelCopiesConfig places an object's copies during the write instead of
// leaving them to the replicator, avoiding the replicator's read-back egress.
// Off by default: it moves the copy work to write time, which can cost PUT
// throughput on a constrained uplink.
//
// Count defaults to replication.factor and may not exceed it. MaxInFlight caps
// the writes whose extra copies are still uploading after the client was
// answered; a write that finds it full places one copy and leaves the rest to
// the replicator. It defaults to how many writes admission lets in at once.
type ParallelCopiesConfig struct {
	Enabled     bool `yaml:"enabled"`
	Count       int  `yaml:"count"`
	MaxInFlight int  `yaml:"max_in_flight"`
}

// MultipartConfig gates the S3 protocol invariants enforced at multipart
// completion. Part-number range, ordering, duplicate and ETag checks are
// always enforced; only the minimum non-final part size is optional, because a
// deployment whose writers split more finely than S3 allows needs it off.
//
// EnforceMinPartSize is a pointer so the loader can tell "absent" (default
// true) from an explicit false.
type MultipartConfig struct {
	EnforceMinPartSize *bool `yaml:"enforce_min_part_size"` // every part but the last must be >= 5 MiB
}

// IsMinPartSizeEnforced returns true unless the operator has explicitly
// disabled the check. The pointer-typed field lets the YAML loader
// distinguish "absent" (default true) from "explicitly false".
func (m *MultipartConfig) IsMinPartSizeEnforced() bool {
	return m.EnforceMinPartSize == nil || *m.EnforceMinPartSize
}

// PendingPatternConfig tunes the reaper that resolves abandoned PUT intents.
// Intents themselves cannot be turned off, since admission subtracts them from
// a backend's headroom while uploads run.
type PendingPatternConfig struct {
	ReaperTick time.Duration `yaml:"reaper_tick"` // How often the reaper resolves abandoned intents (default: 1m)
	MinAge     time.Duration `yaml:"min_age"`     // Don't reap intents younger than this  -  guards in-flight PUTs (default: 5m)
	BatchSize  int           `yaml:"batch_size"`  // Max intents resolved per tick (default: 50)
}

// ReconcileConfig controls the background orphan reconciler that periodically
// scans backends and imports untracked objects into the metadata database.
// Disabled by default.
type ReconcileConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"` // How often to run (default: 24h)
}

// LifecycleConfig holds rules for automatic object expiration. Objects matching
// a rule's filter that are older than expiration_days are deleted by a background
// worker. Empty rules list disables lifecycle processing.
type LifecycleConfig struct {
	Rules     []LifecycleRule `yaml:"rules"`
	BatchSize int             `yaml:"batch_size"` // objects per DB query (default 100)
}

// LifecycleRule defines a single object expiration rule. Prefix and Tags are
// both filters and every one set must match, so a rule carrying each selects
// their intersection. At least one is required: a rule with neither would
// expire the whole namespace. Expressing "or" is a matter of writing a second
// rule, since rules are evaluated independently.
type LifecycleRule struct {
	Prefix         string            `yaml:"prefix"`
	Tags           map[string]string `yaml:"tags"`
	ExpirationDays int               `yaml:"expiration_days"`
}

// filterID renders a rule's filter as a canonical string so two rules that
// select the same objects compare equal regardless of map iteration order.
func (r *LifecycleRule) filterID() string {
	keys := make([]string, 0, len(r.Tags))
	for k := range r.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(r.Prefix)
	for _, k := range keys {
		b.WriteString("\x00")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(r.Tags[k])
	}
	return b.String()
}

// -------------------------------------------------------------------------
// VALIDATION
// -------------------------------------------------------------------------

// Valid reports whether s names a supported strategy.
func (s RoutingStrategy) Valid() bool {
	return s == RoutingPack || s == RoutingSpread
}

// setDefaultsAndValidate fills the rebalancer's defaults whether or not it is
// enabled, since an operator-requested run reads them too, and checks them
// only when the scheduled rebalancer runs.
func (r *RebalanceConfig) setDefaultsAndValidate() []error {
	r.Strategy = cmp.Or(r.Strategy, RoutingPack)
	r.Interval = cmp.Or(r.Interval, 6*time.Hour)
	r.BatchSize = cmp.Or(r.BatchSize, 100)
	r.Threshold = cmp.Or(r.Threshold, 0.1)
	r.Concurrency = cmp.Or(r.Concurrency, 5)
	if !r.Enabled {
		return nil
	}
	errs := failed(
		positive("rebalance.interval", r.Interval),
		positive("rebalance.batch_size", r.BatchSize),
		between("rebalance.threshold", r.Threshold, 0, 1),
		positive("rebalance.concurrency", r.Concurrency),
	)
	if !r.Strategy.Valid() {
		errs = append(errs, fmt.Errorf("rebalance.%w", ErrInvalidRoutingStrategy))
	}
	return errs
}

// setDefaultsAndValidate fills the replicator's defaults and checks them.
// The worker's limits are checked only above factor 1, where it runs.
func (r *ReplicationConfig) setDefaultsAndValidate(backendCount int) []error {
	r.Factor = cmp.Or(r.Factor, 1)
	r.WorkerInterval = cmp.Or(r.WorkerInterval, 5*time.Minute)
	r.BatchSize = cmp.Or(r.BatchSize, 50)
	r.Concurrency = cmp.Or(r.Concurrency, 5)
	r.UnhealthyThreshold = cmp.Or(r.UnhealthyThreshold, 10*time.Minute)
	errs := failed(positive("replication.factor", r.Factor))
	if r.Factor <= 1 {
		return errs
	}
	if r.Factor > backendCount {
		errs = append(errs, fmt.Errorf("%w: factor=%d backends=%d",
			ErrReplicationFactorTooLarge, r.Factor, backendCount))
	}
	return append(errs, failed(
		positive("replication.worker_interval", r.WorkerInterval),
		positive("replication.batch_size", r.BatchSize),
		positive("replication.concurrency", r.Concurrency),
		positive("replication.unhealthy_threshold", r.UnhealthyThreshold),
	)...)
}

// setDefaultsAndValidate fills the cleanup worker's defaults and refuses
// negatives.
func (c *CleanupQueueConfig) setDefaultsAndValidate() []error {
	c.Concurrency = cmp.Or(c.Concurrency, 10)
	c.MultipartStaleTimeout = cmp.Or(c.MultipartStaleTimeout, 24*time.Hour)
	c.ClaimGracePeriod = cmp.Or(c.ClaimGracePeriod, 5*time.Minute)
	return failed(
		positive("cleanup_queue.concurrency", c.Concurrency),
		positive("cleanup_queue.multipart_stale_timeout", c.MultipartStaleTimeout),
		positive("cleanup_queue.claim_grace_period", c.ClaimGracePeriod),
	)
}

// setDefaultsAndValidate fills the pending reaper's defaults and refuses
// negatives.
func (p *PendingPatternConfig) setDefaultsAndValidate() []error {
	p.ReaperTick = cmp.Or(p.ReaperTick, time.Minute)
	p.MinAge = cmp.Or(p.MinAge, 5*time.Minute)
	p.BatchSize = cmp.Or(p.BatchSize, 50)
	return failed(
		positive("write_path.pending_pattern.reaper_tick", p.ReaperTick),
		positive("write_path.pending_pattern.min_age", p.MinAge),
		positive("write_path.pending_pattern.batch_size", p.BatchSize),
	)
}

// setDefaultsAndValidate fills the reconciler's interval and refuses a
// negative one.
func (r *ReconcileConfig) setDefaultsAndValidate() []error {
	r.Interval = cmp.Or(r.Interval, 24*time.Hour)
	return failed(positive("reconcile.interval", r.Interval))
}

// setDefaultsAndValidate fills the lifecycle batch size and checks each rule.
// Duplicates are judged on the whole filter, since two rules sharing a prefix
// but differing by tag select different objects.
func (c *LifecycleConfig) setDefaultsAndValidate() []error {
	c.BatchSize = cmp.Or(c.BatchSize, 100)
	errs := failed(positive("lifecycle.batch_size", c.BatchSize))
	seen := make(map[string]bool)
	for i := range c.Rules {
		r := &c.Rules[i]
		label := fmt.Sprintf("lifecycle.rules[%d]", i)

		errs = append(errs, failed(positive(label+".expiration_days", r.ExpirationDays))...)
		if r.Prefix == "" && len(r.Tags) == 0 {
			errs = append(errs, fmt.Errorf("%s: %w", label, ErrLifecycleFilterRequired))
		}
		if _, empty := r.Tags[""]; empty {
			errs = append(errs, fmt.Errorf("%s: %w", label, ErrLifecycleEmptyTagKey))
		}
		id := r.filterID()
		if seen[id] {
			errs = append(errs, fmt.Errorf("%s: %w: %q", label, ErrDuplicateFilter, id))
		}
		seen[id] = true
	}

	return errs
}

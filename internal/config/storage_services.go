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
	Enabled     bool          `yaml:"enabled"`
	Strategy    string        `yaml:"strategy"` // "pack" or "spread"
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
	Concurrency        int           `yaml:"concurrency"`         // Parallel object replications (default: 5)
	UnhealthyThreshold time.Duration `yaml:"unhealthy_threshold"` // Grace period before replacing copies on circuit-broken backends (default: 10m)
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

// WritePathConfig gates write-path correctness features. The pending-row
// pattern (PUT-before-COMMIT intent tracking) is on by default; operators
// can disable it to fall back to the legacy delete-on-record-failure path,
// which trades data-loss safety for one fewer round-trip per PUT.
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
// the replicator. It defaults to server.max_concurrent_writes.
type ParallelCopiesConfig struct {
	Enabled     bool `yaml:"enabled"`
	Count       int  `yaml:"count"`
	MaxInFlight int  `yaml:"max_in_flight"`
}

// DefaultDetachedUploadCeiling is the in-flight ceiling for a deployment that
// caps neither concurrent writes nor concurrent requests, so there is no
// admission capacity to derive one from. High enough that a fleet keeping up
// never reaches it, low enough to bound what one slow backend can accumulate.
const DefaultDetachedUploadCeiling = 512

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

// setDefaultsAndValidate sets defaults and validate.
func (c *LifecycleConfig) setDefaultsAndValidate() []error {
	if len(c.Rules) == 0 {
		return nil
	}

	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	return nil
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
	r.Factor = cmp.Or(r.Factor, 1)
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
	r.WorkerInterval = cmp.Or(r.WorkerInterval, 5*time.Minute)
	r.BatchSize = cmp.Or(r.BatchSize, 50)
	r.UnhealthyThreshold = cmp.Or(r.UnhealthyThreshold, 10*time.Minute)
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

// validateLifecycleRules enforces that every configured rule has a filter and
// a positive expiration_days. Duplicates are judged on the whole filter, since
// two rules sharing a prefix but differing by tag select different objects.
func validateLifecycleRules(rules []LifecycleRule) []error {
	var errs []error

	seen := make(map[string]bool)
	for i := range rules {
		r := &rules[i]
		label := fmt.Sprintf("lifecycle.rules[%d]", i)

		if r.Prefix == "" && len(r.Tags) == 0 {
			errs = append(errs, fmt.Errorf("%s: %w", label, ErrLifecycleFilterRequired))
		}
		if r.ExpirationDays <= 0 {
			errs = append(errs, fmt.Errorf("%s: %w", label, ErrInvalidExpiration))
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

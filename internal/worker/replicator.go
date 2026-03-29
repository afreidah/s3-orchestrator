// -------------------------------------------------------------------------------
// Replicator - Background Replica Creation Worker
//
// Author: Alex Freidah
//
// Creates additional copies of under-replicated objects across backends. Objects
// are written to one backend on PUT; this worker asynchronously ensures each
// object reaches the configured replication factor. Uses conditional DB inserts
// to safely handle concurrent overwrites and deletes.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// -------------------------------------------------------------------------
// REPLICATOR TYPE
// -------------------------------------------------------------------------

// Replicator creates additional copies of under-replicated objects across
// backends. Embeds *backendCore for access to shared infrastructure.
type Replicator struct {
	ops Ops
	cfg syncutil.AtomicConfig[config.ReplicationConfig]
}

// NewReplicator creates a Replicator that shares the given core infrastructure.
func NewReplicator(ops Ops) *Replicator {
	return &Replicator{ops: ops}
}

// SetConfig atomically stores the replication configuration.
func (r *Replicator) SetConfig(cfg *config.ReplicationConfig) {
	r.cfg.Store(cfg)
}

// Config returns the current replication configuration.
func (r *Replicator) Config() *config.ReplicationConfig {
	return r.cfg.Load()
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Replicate finds under-replicated objects and creates additional copies to
// reach the target replication factor. Returns the number of copies created.
func (r *Replicator) Replicate(ctx context.Context, cfg config.ReplicationConfig) (int, error) {
	start := time.Now()
	ctx = audit.WithRequestID(ctx, audit.NewID())
	ctx, span := telemetry.StartSpan(ctx, "Replicate",
		telemetry.AttrOperation.String("replicate"),
	)
	defer span.End()

	if cfg.Factor <= 1 {
		return 0, nil
	}

	audit.Log(ctx, "replication.start",
		slog.Int("factor", cfg.Factor),
		slog.Int("batch_size", cfg.BatchSize),
	)

	// --- Identify sustained-unhealthy backends ---
	excluded := r.UnhealthyBackends(cfg.UnhealthyThreshold)

	// --- Find under-replicated objects ---
	var locations []store.ObjectLocation
	var err error
	if len(excluded) > 0 {
		locations, err = r.ops.Store().GetUnderReplicatedObjectsExcluding(ctx, cfg.Factor, cfg.BatchSize, excluded)
	} else {
		locations, err = r.ops.Store().GetUnderReplicatedObjects(ctx, cfg.Factor, cfg.BatchSize)
	}
	if err != nil {
		telemetry.ReplicationRunsTotal.WithLabelValues("error").Inc()
		return 0, fmt.Errorf("failed to query under-replicated objects: %w", err)
	}

	if len(locations) == 0 {
		telemetry.ReplicationPending.Set(0)
		telemetry.ReplicationRunsTotal.WithLabelValues("success").Inc()
		telemetry.ReplicationDuration.Observe(time.Since(start).Seconds())
		return 0, nil
	}

	// --- Group locations by object key ---
	grouped := store.GroupByKey(locations)

	// Flatten map into a slice for the worker pool
	type replicaTask struct {
		key    string
		copies []store.ObjectLocation
		needed int
	}
	var tasks []replicaTask
	for key, copies := range grouped {
		needed := cfg.Factor - len(copies)
		if needed > 0 {
			tasks = append(tasks, replicaTask{key: key, copies: copies, needed: needed})
		}
	}

	// --- Fetch quota stats once for the entire cycle ---
	quotaStats, err := r.ops.Store().GetQuotaStats(ctx)
	if err != nil {
		telemetry.ReplicationRunsTotal.WithLabelValues("error").Inc()
		return 0, fmt.Errorf("failed to get quota stats: %w", err)
	}

	// --- Replicate under-replicated objects concurrently ---
	var created atomic.Int32
	workerpool.Run(ctx, cfg.Concurrency, tasks, func(ctx context.Context, task replicaTask) {
		if !r.ops.AcquireAdmission(ctx) {
			return
		}
		defer r.ops.ReleaseAdmission()
		n, replicateErr := r.ReplicateObject(ctx, quotaStats, task.key, task.copies, task.needed)
		if replicateErr != nil {
			slog.WarnContext(ctx, "Replication: object failed", "key", task.key, "error", replicateErr)
		}
		created.Add(int32(n))
	})

	copiesCreated := int(created.Load())
	telemetry.ReplicationCopiesCreatedTotal.Add(float64(copiesCreated))
	if len(excluded) > 0 && copiesCreated > 0 {
		telemetry.ReplicationHealthCopiesTotal.Add(float64(copiesCreated))
	}
	telemetry.ReplicationRunsTotal.WithLabelValues("success").Inc()
	telemetry.ReplicationDuration.Observe(time.Since(start).Seconds())

	audit.Log(ctx, "replication.complete",
		slog.Int("copies_created", copiesCreated),
		slog.Int("objects_checked", len(grouped)),
		slog.Duration("duration", time.Since(start)),
	)

	return copiesCreated, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------


// ReplicateObject creates up to `needed` additional copies of a single object.
// quotaStats is pre-fetched once per replication cycle to avoid redundant DB queries.
// Returns the number of copies successfully created.
func (r *Replicator) ReplicateObject(ctx context.Context, quotaStats map[string]store.QuotaStat, key string, existingCopies []store.ObjectLocation, needed int) (int, error) {
	// Build exclusion set of backends that already hold a copy
	exclusion := make(map[string]bool, len(existingCopies))
	for i := range existingCopies {
		exclusion[existingCopies[i].BackendName] = true
	}

	created := 0
	for i := range needed {
		// --- Find a target backend with space ---
		target := r.FindReplicaTarget(ctx, quotaStats, key, existingCopies[0].SizeBytes, exclusion)
		if target == "" {
			slog.WarnContext(ctx, "Replication: no target backend with space",
				"key", key, "needed", needed-i)
			break
		}

		// --- Copy data from an existing copy to the target ---
		source, err := r.CopyToReplica(ctx, key, existingCopies, target)
		if err != nil {
			slog.WarnContext(ctx, "Replication: failed to copy object data",
				"key", key, "target", target, "error", err)
			telemetry.ReplicationErrorsTotal.Inc()
			continue
		}

		// --- Record the replica in the database (conditional insert) ---
		inserted, err := r.ops.Store().RecordReplica(ctx, key, target, source, existingCopies[0].SizeBytes)
		if err != nil {
			slog.ErrorContext(ctx, "Replication: failed to record replica",
				"key", key, "target", target, "error", err)
			// Clean up orphan on target
			r.CleanupOrphan(ctx, target, key, existingCopies[0].SizeBytes)
			telemetry.ReplicationErrorsTotal.Inc()
			continue
		}

		if !inserted {
			// Source copy was deleted/overwritten during replication
			slog.InfoContext(ctx, "Replication: source copy gone, cleaning up orphan",
				"key", key, "target", target)
			r.CleanupOrphan(ctx, target, key, existingCopies[0].SizeBytes)
			continue
		}

		r.ops.Usage().Record(source, 1, existingCopies[0].SizeBytes, 0) // source: Get + egress
		r.ops.Usage().Record(target, 1, 0, existingCopies[0].SizeBytes) // target: Put + ingress

		audit.Log(ctx, "replication.copy",
			slog.String("key", key),
			slog.String("source_backend", source),
			slog.String("target_backend", target),
			slog.Int64("size", existingCopies[0].SizeBytes),
		)

		exclusion[target] = true
		created++
	}

	return created, nil
}

// FindReplicaTarget selects a backend that has enough space and doesn't already
// hold a copy. quotaStats is pre-fetched once per replication cycle.
// Returns empty string if no suitable target exists.
func (r *Replicator) FindReplicaTarget(ctx context.Context, quotaStats map[string]store.QuotaStat, key string, size int64, exclusion map[string]bool) string {
	for _, name := range r.ops.ExcludeDraining(r.ops.BackendOrder()) {
		if exclusion[name] {
			continue
		}
		if !r.IsBackendHealthy(name) {
			continue
		}
		stat, ok := quotaStats[name]
		if !ok {
			continue
		}
		if stat.BytesLimit-stat.BytesUsed-stat.OrphanBytes >= size {
			return name
		}
	}

	return ""
}

// copyToReplica reads the object from an existing copy and writes it to the
// target backend. Tries each existing copy in order for failover. Returns the
// source backend name that was successfully read from.
func (r *Replicator) CopyToReplica(ctx context.Context, key string, copies []store.ObjectLocation, target string) (string, error) {
	targetBackend, err := r.ops.GetBackend(target)
	if err != nil {
		return "", err
	}

	// Prefer healthy sources to avoid circuit breaker latency/failures.
	slices.SortStableFunc(copies, func(a, b store.ObjectLocation) int {
		aOK := r.IsBackendHealthy(a.BackendName)
		bOK := r.IsBackendHealthy(b.BackendName)
		if aOK == bOK {
			return 0
		}
		if aOK {
			return -1
		}
		return 1
	})

	for i := range copies {
		srcBackend, ok := r.ops.Backends()[copies[i].BackendName]
		if !ok {
			continue
		}

		err := r.ops.StreamCopy(ctx, srcBackend, targetBackend, key)
		if err == nil {
			return copies[i].BackendName, nil
		}

		// Write failures won't improve with a different source — fail immediately.
		if strings.HasPrefix(err.Error(), "write:") {
			return "", fmt.Errorf("failed to write to target %s: %w", target, err)
		}

		slog.WarnContext(ctx, "Replication: source read failed, trying next copy",
			"key", key, "source", copies[i].BackendName, "error", err)
	}

	return "", fmt.Errorf("all source copies failed for key %s", key)
}

// cleanupOrphan deletes an object from a backend when the DB record was not
// created (e.g. source was deleted during replication).
func (r *Replicator) CleanupOrphan(ctx context.Context, backendName, key string, sizeBytes int64) {
	be, ok := r.ops.Backends()[backendName]
	if !ok {
		return
	}
	r.ops.DeleteOrEnqueue(ctx, be, backendName, key, "replication_orphan", sizeBytes)
	r.ops.Usage().Record(backendName, 1, 0, 0)
}

// unhealthyBackends returns backend names whose circuit breakers have been
// open longer than the given threshold. Returns nil when all backends are
// healthy or circuit breakers are not enabled.
func (r *Replicator) UnhealthyBackends(threshold time.Duration) []string {
	var names []string
	for name, be := range r.ops.Backends() {
		cbb, ok := be.(*backend.CircuitBreakerBackend)
		if !ok {
			continue
		}
		if d := cbb.OpenDuration(); d >= threshold {
			names = append(names, name)
			slog.Info("Replication: backend unhealthy, excluding from replica count", //nolint:sloglint // unhealthyBackends has no context
				"backend", name,
				"open_duration", d.Round(time.Second))
		}
	}
	return names
}

// isBackendHealthy returns true if the backend has a closed circuit breaker
// or has no circuit breaker wrapper.
func (r *Replicator) IsBackendHealthy(name string) bool {
	be, ok := r.ops.Backends()[name]
	if !ok {
		return false
	}
	cbb, ok := be.(*backend.CircuitBreakerBackend)
	if !ok {
		return true
	}
	return cbb.IsHealthy()
}

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

package storage

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// -------------------------------------------------------------------------
// REPLICATOR TYPE
// -------------------------------------------------------------------------

// Replicator creates additional copies of under-replicated objects across
// backends. Embeds *backendCore for access to shared infrastructure.
type Replicator struct {
	*backendCore
	cfg atomic.Pointer[config.ReplicationConfig]
}

// NewReplicator creates a Replicator that shares the given core infrastructure.
func NewReplicator(core *backendCore) *Replicator {
	return &Replicator{backendCore: core}
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

	if cfg.Factor <= 1 {
		return 0, nil
	}

	audit.Log(ctx, "replication.start",
		slog.Int("factor", cfg.Factor),
		slog.Int("batch_size", cfg.BatchSize),
	)

	// --- Identify sustained-unhealthy backends ---
	excluded := r.unhealthyBackends(cfg.UnhealthyThreshold)

	// --- Find under-replicated objects ---
	var locations []ObjectLocation
	var err error
	if len(excluded) > 0 {
		locations, err = r.store.GetUnderReplicatedObjectsExcluding(ctx, cfg.Factor, cfg.BatchSize, excluded)
	} else {
		locations, err = r.store.GetUnderReplicatedObjects(ctx, cfg.Factor, cfg.BatchSize)
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
	grouped := groupByKey(locations)

	// --- Replicate each under-replicated object ---
	created := 0
	for key, copies := range grouped {
		needed := cfg.Factor - len(copies)
		if needed <= 0 {
			continue
		}

		n, replicateErr := r.replicateObject(ctx, key, copies, needed)
		if replicateErr != nil {
			slog.Warn("Replication: object failed", "key", key, "error", replicateErr)
		}
		created += n
	}

	telemetry.ReplicationCopiesCreatedTotal.Add(float64(created))
	if len(excluded) > 0 && created > 0 {
		telemetry.ReplicationHealthCopiesTotal.Add(float64(created))
	}
	telemetry.ReplicationRunsTotal.WithLabelValues("success").Inc()
	telemetry.ReplicationDuration.Observe(time.Since(start).Seconds())

	audit.Log(ctx, "replication.complete",
		slog.Int("copies_created", created),
		slog.Int("objects_checked", len(grouped)),
		slog.Duration("duration", time.Since(start)),
	)

	return created, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// groupByKey groups a flat list of object locations into a map keyed by object_key.
func groupByKey(locations []ObjectLocation) map[string][]ObjectLocation {
	grouped := make(map[string][]ObjectLocation)
	for _, loc := range locations {
		grouped[loc.ObjectKey] = append(grouped[loc.ObjectKey], loc)
	}
	return grouped
}

// replicateObject creates up to `needed` additional copies of a single object.
// Returns the number of copies successfully created.
func (r *Replicator) replicateObject(ctx context.Context, key string, existingCopies []ObjectLocation, needed int) (int, error) {
	// Build exclusion set of backends that already hold a copy
	exclusion := make(map[string]bool, len(existingCopies))
	for _, c := range existingCopies {
		exclusion[c.BackendName] = true
	}

	created := 0
	for i := 0; i < needed; i++ {
		// --- Find a target backend with space ---
		target := r.findReplicaTarget(ctx, key, existingCopies[0].SizeBytes, exclusion)
		if target == "" {
			slog.Warn("Replication: no target backend with space",
				"key", key, "needed", needed-i)
			break
		}

		// --- Copy data from an existing copy to the target ---
		source, err := r.copyToReplica(ctx, key, existingCopies, target)
		if err != nil {
			slog.Warn("Replication: failed to copy object data",
				"key", key, "target", target, "error", err)
			telemetry.ReplicationErrorsTotal.Inc()
			continue
		}

		// --- Record the replica in the database (conditional insert) ---
		inserted, err := r.store.RecordReplica(ctx, key, target, source, existingCopies[0].SizeBytes)
		if err != nil {
			slog.Error("Replication: failed to record replica",
				"key", key, "target", target, "error", err)
			// Clean up orphan on target
			r.cleanupOrphan(ctx, target, key)
			telemetry.ReplicationErrorsTotal.Inc()
			continue
		}

		if !inserted {
			// Source copy was deleted/overwritten during replication
			slog.Info("Replication: source copy gone, cleaning up orphan",
				"key", key, "target", target)
			r.cleanupOrphan(ctx, target, key)
			continue
		}

		r.usage.Record(source, 1, existingCopies[0].SizeBytes, 0) // source: Get + egress
		r.usage.Record(target, 1, 0, existingCopies[0].SizeBytes) // target: Put + ingress

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

// findReplicaTarget selects a backend that has enough space and doesn't already
// hold a copy. Returns empty string if no suitable target exists.
func (r *Replicator) findReplicaTarget(ctx context.Context, key string, size int64, exclusion map[string]bool) string {
	stats, err := r.store.GetQuotaStats(ctx)
	if err != nil {
		slog.Warn("Replication: failed to get quota stats", "error", err)
		return ""
	}

	for _, name := range r.excludeDraining(r.order) {
		if exclusion[name] {
			continue
		}
		if !r.isBackendHealthy(name) {
			continue
		}
		stat, ok := stats[name]
		if !ok {
			continue
		}
		if stat.BytesLimit-stat.BytesUsed >= size {
			return name
		}
	}

	return ""
}

// copyToReplica reads the object from an existing copy and writes it to the
// target backend. Tries each existing copy in order for failover. Returns the
// source backend name that was successfully read from.
func (r *Replicator) copyToReplica(ctx context.Context, key string, copies []ObjectLocation, target string) (string, error) {
	targetBackend, err := r.getBackend(target)
	if err != nil {
		return "", err
	}

	// Prefer healthy sources to avoid circuit breaker latency/failures.
	slices.SortStableFunc(copies, func(a, b ObjectLocation) int {
		aOK := r.isBackendHealthy(a.BackendName)
		bOK := r.isBackendHealthy(b.BackendName)
		if aOK == bOK {
			return 0
		}
		if aOK {
			return -1
		}
		return 1
	})

	for _, cp := range copies {
		srcBackend, ok := r.backends[cp.BackendName]
		if !ok {
			continue
		}

		err := r.streamCopy(ctx, srcBackend, targetBackend, key)
		if err == nil {
			return cp.BackendName, nil
		}

		// Write failures won't improve with a different source — fail immediately.
		if strings.HasPrefix(err.Error(), "write:") {
			return "", fmt.Errorf("failed to write to target %s: %w", target, err)
		}

		slog.Warn("Replication: source read failed, trying next copy",
			"key", key, "source", cp.BackendName, "error", err)
	}

	return "", fmt.Errorf("all source copies failed for key %s", key)
}

// cleanupOrphan deletes an object from a backend when the DB record was not
// created (e.g. source was deleted during replication).
func (r *Replicator) cleanupOrphan(ctx context.Context, backendName, key string) {
	backend, ok := r.backends[backendName]
	if !ok {
		return
	}
	r.deleteOrEnqueue(ctx, backend, backendName, key, "replication_orphan")
	r.usage.Record(backendName, 1, 0, 0)
}

// unhealthyBackends returns backend names whose circuit breakers have been
// open longer than the given threshold. Returns nil when all backends are
// healthy or circuit breakers are not enabled.
func (r *Replicator) unhealthyBackends(threshold time.Duration) []string {
	var names []string
	for name, backend := range r.backends {
		cbb, ok := backend.(*CircuitBreakerBackend)
		if !ok {
			continue
		}
		if d := cbb.OpenDuration(); d >= threshold {
			names = append(names, name)
			slog.Info("Replication: backend unhealthy, excluding from replica count",
				"backend", name,
				"open_duration", d.Round(time.Second))
		}
	}
	return names
}

// isBackendHealthy returns true if the backend has a closed circuit breaker
// or has no circuit breaker wrapper.
func (r *Replicator) isBackendHealthy(name string) bool {
	backend, ok := r.backends[name]
	if !ok {
		return false
	}
	cbb, ok := backend.(*CircuitBreakerBackend)
	if !ok {
		return true
	}
	return cbb.IsHealthy()
}

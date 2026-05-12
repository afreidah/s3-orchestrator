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
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// -------------------------------------------------------------------------
// REPLICATOR TYPE
// -------------------------------------------------------------------------

// Replicator creates additional copies of under-replicated objects across backends.
type Replicator struct {
	log   *slog.Logger
	ops   Ops
	store ReplicatorStore
	cfg   syncutil.AtomicConfig[config.ReplicationConfig]
}

// NewReplicator creates a Replicator with fleet operations and a narrow store.
func NewReplicator(ops Ops, store ReplicatorStore) *Replicator {
	return &Replicator{ops: ops, store: store, log: slog.Default().With(logfmt.Component("replicator"))}
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
	return runOpsCycle(ctx, "Replicate", "replicate", func(ctx context.Context) (int, error) {
		return r.replicate(ctx, cfg)
	})
}

// replicate is the body of Replicate after observe.Run sets up the span.
func (r *Replicator) replicate(ctx context.Context, cfg config.ReplicationConfig) (int, error) {
	start := time.Now()
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
	var locations []core.ObjectLocation
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
	grouped := core.GroupByKey(locations)

	// Flatten map into a slice for the worker pool
	type replicaTask struct {
		key    string
		copies []core.ObjectLocation
		needed int
	}
	var tasks []replicaTask
	for key, copies := range grouped {
		needed := cfg.Factor - len(copies)
		if needed > 0 {
			tasks = append(tasks, replicaTask{key: key, copies: copies, needed: needed})
		}
	}

	// Target selection runs through SelectReplicaTarget on every object;
	// the backend manager filters on in-memory usage limits and queries the
	// store for least-utilized / first-with-space backend per call. Over-
	// quota races are caught by the backend layer (RecordReplica returns an
	// error) so the worst case is a wasted copy that gets cleaned up.
	telemetry.ReplicationPending.Set(float64(len(tasks)))
	var created atomic.Int32
	workerpool.Run(ctx, cfg.Concurrency, tasks, func(ctx context.Context, task replicaTask) {
		defer telemetry.ReplicationPending.Dec()
		WithAdmission(ctx, r.ops, WorkerNameReplicator, func() {
			n, replicateErr := r.ReplicateObject(ctx, task.key, task.copies, task.needed)
			if replicateErr != nil {
				r.log.WarnContext(ctx, "object failed", "key", task.key, "error", replicateErr)
			}
			created.Add(int32(n)) //nolint:gosec // G115: n is copies created per object, always small
		})
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
// Returns the number of copies successfully created.
func (r *Replicator) ReplicateObject(ctx context.Context, key string, existingCopies []core.ObjectLocation, needed int) (int, error) {
	// Build exclusion set of backends that already hold a copy
	exclusion := make(map[string]bool, len(existingCopies))
	for i := range existingCopies {
		exclusion[existingCopies[i].BackendName] = true
	}

	// Estimate size for target selection. Selection runs before we pick a
	// source, so we use the largest known copy size to avoid placing on a
	// backend that lacks space for the (likely identical) copy that will
	// be transferred. Authoritative size for quota and metadata is the
	// source's size at insert time, returned by RecordReplica below.
	sizeEstimate := maxCopySize(existingCopies)

	// Retry with different targets on failure without consuming a needed
	// slot. Cap total attempts to avoid unbounded retries when every
	// remaining backend rejects the object.
	created := 0
	maxAttempts := needed + len(r.ops.Backends())
	for attempt := 0; created < needed && attempt < maxAttempts; attempt++ {
		remaining := needed - created
		target := r.FindReplicaTarget(ctx, key, sizeEstimate, exclusion)
		if target == "" {
			r.log.WarnContext(ctx, "no target backend with space",
				"key", key, "needed", remaining)
			break
		}

		// CopyToReplica returns the source's size from the in-memory
		// ObjectLocation slice. transferredSize is the bytes the
		// streaming copy moved; recordedSize is what RecordReplica
		// actually wrote into both object_locations.size_bytes and
		// backend_quotas.bytes_used (read from the source row inside
		// the conditional INSERT). They equal each other unless an
		// overwrite landed mid-replication.
		source, transferredSize, err := r.CopyToReplica(ctx, key, existingCopies, target)
		if err != nil {
			r.log.WarnContext(ctx, "failed to copy object data",
				"key", key, "target", target, "error", err)
			telemetry.ReplicationErrorsTotal.Inc()
			exclusion[target] = true
			continue
		}

		recordedSize, inserted, err := r.store.RecordReplica(ctx, key, target, source)
		if err != nil {
			r.log.ErrorContext(ctx, "failed to record replica",
				"key", key, "target", target, "error", err)
			r.CleanupOrphan(ctx, target, key, transferredSize)
			telemetry.ReplicationErrorsTotal.Inc()
			exclusion[target] = true
			continue
		}

		if !inserted {
			// Source copy was deleted/overwritten during replication.
			r.log.InfoContext(ctx, "source copy gone, cleaning up orphan",
				"key", key, "target", target)
			r.CleanupOrphan(ctx, target, key, transferredSize)
			exclusion[target] = true
			continue
		}

		// Surface the rare race where the source row's size_bytes shifted
		// between GetUnderReplicatedObjects and the conditional INSERT.
		// recordedSize is authoritative for accounting; the operator
		// sees the discrepancy in case it indicates a deeper bug.
		if recordedSize != transferredSize {
			r.log.WarnContext(ctx, "source size shifted mid-replication",
				"key", key, "source", source, "transferred", transferredSize, "recorded", recordedSize)
		}

		r.ops.Usage().Record(source, 1, recordedSize, 0) // source: Get + egress
		r.ops.Usage().Record(target, 1, 0, recordedSize) // target: Put + ingress

		audit.Log(ctx, "replication.copy",
			slog.String("key", key),
			slog.String("source_backend", source),
			slog.String("target_backend", target),
			slog.Int64("size", recordedSize),
		)

		exclusion[target] = true
		created++
	}

	return created, nil
}

// maxCopySize returns the largest SizeBytes across copies, used as a
// conservative size estimate for target selection before a source is
// picked. Returns 0 for an empty slice.
func maxCopySize(copies []core.ObjectLocation) int64 {
	var m int64
	for i := range copies {
		if copies[i].SizeBytes > m {
			m = copies[i].SizeBytes
		}
	}
	return m
}

// FindReplicaTarget selects a backend for a replication copy using the same
// routing strategy as normal writes. Returns empty string if no suitable
// target exists.
func (r *Replicator) FindReplicaTarget(ctx context.Context, key string, size int64, exclusion map[string]bool) string {
	name, err := r.ops.SelectReplicaTarget(ctx, size, exclusion)
	if err != nil {
		r.log.WarnContext(ctx, "target selection failed",
			"key", key, "error", err)
		return ""
	}
	return name
}

// copyToReplica reads the object from an existing copy and writes it to the
// target backend. Tries each existing copy in order for failover. Returns the
// source backend name that was successfully read from and the size_bytes
// recorded on that source's ObjectLocation row (the size of the bytes that
// were actually transferred).
func (r *Replicator) CopyToReplica(ctx context.Context, key string, copies []core.ObjectLocation, target string) (string, int64, error) {
	targetBackend, err := r.ops.GetBackend(target)
	if err != nil {
		return "", 0, err
	}

	// Prefer healthy sources to avoid circuit breaker latency/failures.
	slices.SortStableFunc(copies, func(a, b core.ObjectLocation) int {
		return cmpHealthFirst(r.IsBackendHealthy(a.BackendName), r.IsBackendHealthy(b.BackendName))
	})

	for i := range copies {
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		sourceName, sourceSize, terminal, err := r.tryCopyFrom(ctx, key, target, targetBackend, &copies[i])
		if terminal {
			return sourceName, sourceSize, err
		}
	}

	return "", 0, fmt.Errorf("all source copies failed for key %s", key)
}

// cmpHealthFirst orders two health flags so true (healthy) sorts before
// false (unhealthy). The comparator inside CopyToReplica delegates to
// this helper so the closure body stays a single expression and the
// outer method stays under the cognitive-complexity ceiling.
func cmpHealthFirst(aOK, bOK bool) int {
	switch {
	case aOK == bOK:
		return 0
	case aOK:
		return -1
	default:
		return 1
	}
}

// tryCopyFrom attempts a stream-copy from one source location to the
// target. Returns terminal=true with (sourceName, sourceSize, nil) on
// success or with ("", 0, err) when the failure mode means no other
// source could help (a write-side error). Returns terminal=false to
// signal the caller should move on to the next source.
func (r *Replicator) tryCopyFrom(ctx context.Context, key, target string, targetBackend backend.ObjectBackend, loc *core.ObjectLocation) (string, int64, bool, error) {
	srcBackend, ok := r.ops.Backends()[loc.BackendName]
	if !ok {
		return "", 0, false, nil
	}
	err := r.ops.StreamCopy(ctx, srcBackend, targetBackend, key)
	if err == nil {
		return loc.BackendName, loc.SizeBytes, true, nil
	}
	// Write failures won't improve with a different source  -  fail immediately.
	if strings.HasPrefix(err.Error(), "write:") {
		return "", 0, true, fmt.Errorf("failed to write to target %s: %w", target, err)
	}
	r.log.WarnContext(ctx, "source read failed, trying next copy",
		"key", key, "source", loc.BackendName, "error", err)
	if isNotFound(err) {
		r.pruneStaleSource(ctx, key, loc.BackendName)
	}
	return "", 0, false, nil
}

// pruneStaleSource removes a source-side ObjectLocation row when the
// backend reported the object missing. Logging-only on DB failure: a
// stuck stale row is preferable to aborting the replication pass.
func (r *Replicator) pruneStaleSource(ctx context.Context, key, backendName string) {
	if delErr := r.store.DeleteObjectLocation(ctx, key, backendName); delErr != nil {
		r.log.WarnContext(ctx, "failed to remove stale metadata",
			"key", key, "backend", backendName, "error", delErr)
		return
	}
	r.log.InfoContext(ctx, "removed stale metadata entry",
		"key", key, "backend", backendName)
}

// CleanupOrphan deletes an object from a backend when the DB record was not
// created (e.g. source was deleted during replication). Looks up the
// backend by name and dispatches to DeleteOrEnqueue, which handles its
// own API accounting and orphan-byte tracking.
func (r *Replicator) CleanupOrphan(ctx context.Context, backendName, key string, sizeBytes int64) {
	be, ok := r.ops.Backends()[backendName]
	if !ok {
		return
	}
	r.ops.DeleteOrEnqueue(ctx, be, backendName, key, "replication_orphan", sizeBytes)
}

// UnhealthyBackends returns backend names whose circuit breakers have been
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
			slog.InfoContext(context.Background(), "backend unhealthy, excluding from replica count",
				"backend", name,
				"open_duration", d.Round(time.Second))
		}
	}
	return names
}

// IsBackendHealthy returns true if the backend has a closed circuit breaker
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

// isNotFound returns true if the error chain contains an HTTP 404 response,
// indicating the object does not exist on the backend.
func isNotFound(err error) bool {
	var respErr interface{ HTTPStatusCode() int }
	return errors.As(err, &respErr) && respErr.HTTPStatusCode() == 404
}

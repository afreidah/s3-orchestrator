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
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// -------------------------------------------------------------------------
// REPLICATOR TYPE
// -------------------------------------------------------------------------

// ReplicatorStore is the narrow persistence surface the replicator needs:
// under-replicated discovery + per-replica record / location updates.
// Declared locally so the worker does not pull in the full MetadataStore.
type ReplicatorStore interface {
	core.ObjectStore
	core.ReplicationStore
}

// Replicator creates additional copies of under-replicated objects across backends.
type Replicator struct {
	log       *slog.Logger
	ops       Ops
	placement Placement
	store     ReplicatorStore
	cfg       syncutil.AtomicConfig[config.ReplicationConfig]
}

// NewReplicator creates a Replicator with fleet operations, write-path
// placement, and a metadata store.
func NewReplicator(ops Ops, placement Placement, store ReplicatorStore) *Replicator {
	must.NotNil("ops", ops)
	must.NotNil("placement", placement)
	must.NotNil("store", store)
	return &Replicator{ops: ops, placement: placement, store: store, log: slog.Default().With(logfmt.Component("replicator"))}
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
// observer, when non-nil, receives a start and end step per object replicated.
func (r *Replicator) Replicate(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (int, error) {
	return runOpsCycle(ctx, "Replicate", "replicate", func(ctx context.Context) (int, error) {
		return r.replicate(ctx, cfg, observer)
	})
}

// replicate is the body of Replicate after observe.Run sets up the span.
func (r *Replicator) replicate(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (int, error) {
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

	tasks := planUnderReplicated(locations, cfg.Factor)

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
			progress.Track(observer, task.key, func() string {
				outcome := r.ReplicateObject(ctx, task.key, task.copies, task.needed)
				r.reportObjectOutcome(ctx, &outcome)
				created.Add(int32(outcome.Created)) //nolint:gosec // G115: Created is small per object
				if outcome.Failed() > 0 {
					return progress.StatusFailed
				}
				return progress.StatusOK
			})
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
		slog.Int("objects_checked", len(tasks)),
		slog.Duration("duration", time.Since(start)),
	)

	return copiesCreated, nil
}

// -------------------------------------------------------------------------
// PLANNER
// -------------------------------------------------------------------------

// replicaTask is the unit of work the replicator's fan-out processes:
// one object key that needs `needed` additional copies, with the
// existing copies the executor will read from.
type replicaTask struct {
	key    string
	copies []core.ObjectLocation
	needed int
}

// planUnderReplicated turns a flat slice of object_locations rows (one
// row per backend per key) into per-key replication tasks targeting
// `factor` total copies. Keys that already meet or exceed the factor
// are filtered out; the returned slice ordering is map-iteration order
// and not guaranteed to be stable across runs. Pure function so the
// placement/policy decisions are testable independently of backend
// copy execution.
func planUnderReplicated(locations []core.ObjectLocation, factor int) []replicaTask {
	grouped := core.GroupByKey(locations)
	tasks := make([]replicaTask, 0, len(grouped))
	for key, copies := range grouped {
		needed := factor - len(copies)
		if needed > 0 {
			tasks = append(tasks, replicaTask{key: key, copies: copies, needed: needed})
		}
	}
	return tasks
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// ReplicationOutcome captures the per-object result of one
// ReplicateObject invocation. Counts are populated regardless of
// success so the reporter and unit tests can reason about retry
// behaviour without parsing log lines. NoTarget reflects whether the
// loop exited because target selection ran out, not whether any
// individual attempt failed.
type ReplicationOutcome struct {
	Key          string // object key
	Created      int    // copies successfully recorded
	CopyErrors   int    // source -> target stream copies that errored
	RecordErrors int    // RecordReplica failures (after the copy succeeded)
	Superseded   int    // RecordReplica returned inserted=false (source row gone)
	NoTarget     bool   // selection failed before `needed` was reached
}

// Failed reports the total number of failed copy attempts for this
// object, irrespective of failure stage.
func (o ReplicationOutcome) Failed() int {
	return o.CopyErrors + o.RecordErrors + o.Superseded
}

// ReplicateObject creates up to `needed` additional copies of a single
// object. Returns a ReplicationOutcome the caller can use to drive
// metrics, audit, and log reporting without re-parsing logs.
//
// Per-attempt diagnostic logs are preserved so incident responders can
// still trace each failed retry; the outcome is a *structured summary*
// on top of those, not a replacement.
func (r *Replicator) ReplicateObject(ctx context.Context, key string, existingCopies []core.ObjectLocation, needed int) ReplicationOutcome {
	out := ReplicationOutcome{Key: key}

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
	maxAttempts := needed + len(r.ops.Backends())
	for attempt := 0; out.Created < needed && attempt < maxAttempts; attempt++ {
		remaining := needed - out.Created
		target := r.FindReplicaTarget(ctx, key, sizeEstimate, exclusion)
		if target == "" {
			r.log.WarnContext(ctx, "no target backend with space",
				"key", key, "needed", remaining)
			out.NoTarget = true
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
			out.CopyErrors++
			continue
		}

		recordedSize, inserted, err := r.store.RecordReplica(ctx, key, target, source)
		if err != nil {
			r.log.ErrorContext(ctx, "failed to record replica",
				"key", key, "target", target, "error", err)
			r.CleanupOrphan(ctx, target, key, transferredSize)
			telemetry.ReplicationErrorsTotal.Inc()
			exclusion[target] = true
			out.RecordErrors++
			continue
		}

		if !inserted {
			// Source copy was deleted/overwritten during replication.
			r.log.InfoContext(ctx, "source copy gone, cleaning up orphan",
				"key", key, "target", target)
			r.CleanupOrphan(ctx, target, key, transferredSize)
			exclusion[target] = true
			out.Superseded++
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

		r.ops.Acct().Egress(source, recordedSize)
		r.ops.Acct().Ingress(target, recordedSize)

		audit.Log(ctx, "replication.copy",
			slog.String("key", key),
			slog.String("src_backend", source),
			slog.String("target_backend", target),
			slog.Int64("size", recordedSize),
		)

		exclusion[target] = true
		out.Created++
	}

	return out
}

// reportObjectOutcome drives the summary log line emitted by the
// run-loop closure once ReplicateObject returns. Per-attempt
// diagnostic logs are emitted inside ReplicateObject; this helper adds
// the aggregate "this object did not reach factor" signal so dashboards
// and operators see one structured entry per partial outcome rather
// than having to count per-attempt warnings. No-failure outcomes emit
// nothing  -  the bulk-summary log lives in replicate() above.
func (r *Replicator) reportObjectOutcome(ctx context.Context, o *ReplicationOutcome) {
	if o.Failed() == 0 && !o.NoTarget {
		return
	}
	r.log.WarnContext(ctx, "object did not reach replication factor",
		"key", o.Key,
		"created", o.Created,
		"copy_errors", o.CopyErrors,
		"record_errors", o.RecordErrors,
		"superseded", o.Superseded,
		"no_target", o.NoTarget,
	)
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
	name, err := r.placement.SelectReplicaTarget(ctx, size, exclusion)
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
// were actually transferred). The input slice is cloned before sorting so
// callers retain their original ordering — without the clone, sort.Slice
// reorders the caller's slice in place and the caller's later reads see
// a different element at each index.
func (r *Replicator) CopyToReplica(ctx context.Context, key string, copies []core.ObjectLocation, target string) (string, int64, error) {
	targetBackend, err := r.ops.GetBackend(target)
	if err != nil {
		return "", 0, err
	}

	// Prefer healthy sources to avoid circuit breaker latency/failures.
	// Sort a clone so the caller's slice keeps the order they passed
	// in - this method does not advertise in-place mutation and the
	// outer ReplicateObject loop reuses the same existingCopies slice
	// across iterations.
	ordered := slices.Clone(copies)
	slices.SortStableFunc(ordered, func(a, b core.ObjectLocation) int {
		return cmpHealthFirst(r.IsBackendHealthy(a.BackendName), r.IsBackendHealthy(b.BackendName))
	})

	// Drop sources whose breaker is open so a streamed copy never starts
	// against a backend we already know is down - a slow or dead source
	// otherwise stalls the copy and (before the phase-attribution fix) trips
	// the healthy target's breaker. Fall back to the full set only when no
	// source is currently healthy, so an object whose only copies sit on
	// degraded backends still gets an attempt rather than being stranded.
	candidates := make([]core.ObjectLocation, 0, len(ordered))
	for i := range ordered {
		if r.IsBackendHealthy(ordered[i].BackendName) {
			candidates = append(candidates, ordered[i])
		}
	}
	if len(candidates) == 0 {
		candidates = ordered
	}

	for i := range candidates {
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		sourceName, sourceSize, terminal, err := r.tryCopyFrom(ctx, key, target, targetBackend, &candidates[i])
		if terminal {
			return sourceName, sourceSize, err
		}
	}

	return "", 0, fmt.Errorf("all source copies failed for key %s", key)
}

// cmpHealthFirst orders two health flags so true (healthy) sorts
// before false (unhealthy).
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
// signal the caller should move on to the next source. Failure
// classification is structural: a *backend.CopyError with
// CopyPhaseWrite is terminal, anything else (CopyPhaseRead or an
// untyped error) retries the next source.
func (r *Replicator) tryCopyFrom(ctx context.Context, key, target string, targetBackend backend.ObjectBackend, loc *core.ObjectLocation) (string, int64, bool, error) {
	srcBackend, ok := r.ops.Backends()[loc.BackendName]
	if !ok {
		return "", 0, false, nil
	}
	err := r.ops.StreamCopy(ctx, srcBackend, targetBackend, key)
	if err == nil {
		return loc.BackendName, loc.SizeBytes, true, nil
	}
	// Write failures won't improve with a different source - fail immediately.
	if backend.IsCopyPhase(err, backend.CopyPhaseWrite) {
		return "", 0, true, fmt.Errorf("failed to write to target %s: %w", target, err)
	}
	r.log.WarnContext(ctx, "source read failed, trying next copy",
		"key", key, "source", loc.BackendName, "error", err)
	if backend.IsNotFound(err) {
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
	r.placement.DeleteOrEnqueue(ctx, be, backendName, key, "replication_orphan", sizeBytes)
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
			r.log.InfoContext(context.Background(), "backend unhealthy, excluding from replica count",
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

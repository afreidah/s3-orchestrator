// -------------------------------------------------------------------------------
// Replicator - Background Replica Creation Worker
//
// Author: Alex Freidah
//
// Creates additional copies of under-replicated objects across backends. Objects
// are written to one backend on PUT; this worker asynchronously ensures each
// object reaches the configured replication factor. Uses conditional DB inserts
// to safely handle concurrent overwrites and deletes.
//
// When integrity.verify_on_replicate is on, each new copy is read back and
// hash-checked before it is recorded; replicator_verify.go holds that path.
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
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
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
	hasher    *storedHasher
	cfg       syncutil.AtomicConfig[config.ReplicationConfig]
	integrity syncutil.AtomicConfig[config.IntegrityConfig]
}

// ReplicatorDeps groups the replicator's constructor dependencies. Encryptor
// and Codec are optional and are only consulted when integrity.verify_on_replicate
// is on: verifying a copy means undoing its stored form before hashing, so a
// deployment that stores encrypted or compressed objects needs the matching one.
type ReplicatorDeps struct {
	Ops       Ops
	Placement Placement
	Store     ReplicatorStore
	Encryptor *encryption.Encryptor
	Codec     StreamDecompressor
}

// NewReplicator creates a Replicator with the given dependencies.
func NewReplicator(deps ReplicatorDeps) *Replicator {
	must.NotNil("Ops", deps.Ops)
	must.NotNil("Placement", deps.Placement)
	must.NotNil("Store", deps.Store)
	return &Replicator{
		ops:       deps.Ops,
		placement: deps.Placement,
		store:     deps.Store,
		hasher:    newStoredHasher(deps.Ops, deps.Encryptor, deps.Codec, "replicator"),
		log:       slog.Default().With(logfmt.Component("replicator")),
	}
}

// SetConfig atomically stores the replication configuration.
func (r *Replicator) SetConfig(cfg *config.ReplicationConfig) {
	r.cfg.Store(cfg)
}

// Config returns the current replication configuration.
func (r *Replicator) Config() *config.ReplicationConfig {
	return r.cfg.Load()
}

// SetIntegrityConfig atomically stores the integrity configuration, which
// decides whether a new copy is read back and hash-checked before it is
// recorded.
func (r *Replicator) SetIntegrityConfig(cfg *config.IntegrityConfig) {
	r.integrity.Store(cfg)
}

// IntegrityConfig returns the current integrity configuration.
func (r *Replicator) IntegrityConfig() *config.IntegrityConfig {
	return r.integrity.Load()
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ReplicationSummary is the outcome of one replication cycle: the per-item
// tally every worker reports, plus the copies those items created. The two
// counts differ because one under-replicated object can need several copies,
// so CopiesCreated is not the number of items that succeeded.
type ReplicationSummary struct {
	batch.Summary
	CopiesCreated int
}

// Replicate finds under-replicated objects and creates additional copies to
// reach the target replication factor. observer, when non-nil, receives a
// start and end step per object replicated.
func (r *Replicator) Replicate(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (ReplicationSummary, error) {
	return runOpsCycle(ctx, "Replicate", "replicate", func(ctx context.Context) (ReplicationSummary, error) {
		return r.replicate(ctx, cfg, observer)
	})
}

// replicate is the body of Replicate after observe.Run sets up the span.
func (r *Replicator) replicate(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (ReplicationSummary, error) {
	start := time.Now()
	if cfg.Factor <= 1 {
		return ReplicationSummary{}, nil
	}

	audit.Log(ctx, "replication.start",
		slog.Int("factor", cfg.Factor),
		slog.Int("batch_size", cfg.BatchSize),
	)

	excluded := r.UnhealthyBackends(cfg.UnhealthyThreshold)

	var locations []core.ObjectLocation
	var err error
	if len(excluded) > 0 {
		locations, err = r.store.GetUnderReplicatedObjectsExcluding(ctx, cfg.Factor, cfg.BatchSize, excluded)
	} else {
		locations, err = r.store.GetUnderReplicatedObjects(ctx, cfg.Factor, cfg.BatchSize)
	}
	if err != nil {
		telemetry.ReplicationRunsTotal.WithLabelValues(batch.OutcomeError).Inc()
		return ReplicationSummary{}, fmt.Errorf("failed to query under-replicated objects: %w", err)
	}

	if len(locations) == 0 {
		telemetry.ReplicationRunsTotal.WithLabelValues(batch.Summary{}.Outcome()).Inc()
		telemetry.ReplicationDuration.Observe(time.Since(start).Seconds())
		return ReplicationSummary{}, nil
	}

	tasks := planUnderReplicated(locations, cfg.Factor)

	// The pending gauge comes from the fleet snapshot's ledger count, not this
	// batch, so every instance serves the same figure.
	var created atomic.Int64
	runner := batch.Runner[replicaTask]{
		Name:        "replication",
		Log:         r.log,
		Concurrency: cfg.Concurrency,
		Observer:    observer,
		Key:         func(t replicaTask) string { return t.key },
	}
	sum := runner.Run(ctx, tasks, func(ctx context.Context, task replicaTask) batch.ItemResult {
		var res batch.ItemResult // zero value (batch.ItemSkipped) when admission blocks the work
		WithAdmission(ctx, r.ops, WorkerNameReplicator, func() {
			outcome := r.ReplicateObject(ctx, task.key, task.copies, task.needed)
			r.reportObjectOutcome(ctx, &outcome)
			created.Add(int64(outcome.Created))
			res = replicaOutcomeResult(&outcome)
		})
		return res
	})

	summary := ReplicationSummary{Summary: sum, CopiesCreated: int(created.Load())}
	telemetry.ReplicationCopiesCreatedTotal.Add(float64(summary.CopiesCreated))
	if len(excluded) > 0 && summary.CopiesCreated > 0 {
		telemetry.ReplicationHealthCopiesTotal.Add(float64(summary.CopiesCreated))
	}
	telemetry.ReplicationRunsTotal.WithLabelValues(sum.Outcome()).Inc()
	telemetry.ReplicationDuration.Observe(time.Since(start).Seconds())

	audit.Log(ctx, "replication.complete",
		slog.Int("copies_created", summary.CopiesCreated),
		slog.Int("objects_checked", len(tasks)),
		slog.Int("objects_failed", sum.Failed),
		slog.Duration("duration", time.Since(start)),
	)

	return summary, nil
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

// planUnderReplicated turns object_locations rows (one per backend per key)
// into per-key replication tasks targeting factor total copies. Keys already
// at or above the factor are dropped; the result order is not stable.
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

// ReplicationOutcome captures the per-object result of one ReplicateObject
// call. NoTarget means the loop exited because target selection ran out, not
// that an individual attempt failed.
type ReplicationOutcome struct {
	Key             string // object key
	Created         int    // copies successfully recorded
	CopyErrors      int    // source -> target stream copies that errored
	RecordErrors    int    // RecordReplica failures (after the copy succeeded)
	Superseded      int    // RecordReplica returned inserted=false (source row gone)
	VerifyMismatch  int    // copies discarded because their hash disagreed with the source
	VerifyUnchecked int    // copies recorded without a hash check that was asked for
	NoTarget        bool   // selection failed before `needed` was reached
}

// Failed reports the total number of failed copy attempts for this
// object, irrespective of failure stage. VerifyUnchecked is not a failure: the
// copy was written and recorded, it simply could not be checked.
func (o ReplicationOutcome) Failed() int {
	return o.CopyErrors + o.RecordErrors + o.Superseded + o.VerifyMismatch
}

// replicaOutcomeResult folds one object's copy attempts into the batch tally.
// Any failed attempt makes the item failed even when other copies of the same
// object landed, because the object did not reach its factor: reporting it as
// succeeded would let a cycle that left objects under-replicated read as clean.
func replicaOutcomeResult(o *ReplicationOutcome) batch.ItemResult {
	switch {
	case o.Failed() > 0:
		return batch.ItemResult{Outcome: batch.ItemFailed, Status: progress.StatusFailed}
	case o.Created > 0:
		return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: progress.StatusOK}
	default:
		// No copy was attempted: target selection found nowhere to put one.
		return batch.ItemResult{Outcome: batch.ItemSkipped, Status: progress.StatusOK}
	}
}

// ReplicateObject creates up to `needed` additional copies of a single
// object. It logs each failed attempt and returns a ReplicationOutcome
// summarizing them for metrics, audit, and reporting.
func (r *Replicator) ReplicateObject(ctx context.Context, key string, existingCopies []core.ObjectLocation, needed int) ReplicationOutcome {
	out := ReplicationOutcome{Key: key}

	// Build exclusion set of backends that already hold a copy
	exclusion := make(map[string]bool, len(existingCopies))
	for i := range existingCopies {
		exclusion[existingCopies[i].BackendName] = true
	}

	// Target selection runs before a source is picked, so it uses the largest
	// known copy size. The authoritative size is the one RecordReplica reads
	// from the source row at insert time.
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
			event.Publish(event.ReplicationTargetExhausted, key, map[string]any{
				"key":               key,
				"copies_needed":     remaining,
				"copies_created":    out.Created,
				"size_bytes":        sizeEstimate,
				"excluded_backends": exclusion,
			})
			out.NoTarget = true
			break
		}

		// No capacity claim is held across the copy: RecordReplica inserts the
		// row only if the target still has room, so a failed attempt has
		// nothing to give back. The target path is minted first so the verify
		// read, the record, and every cleanup address the same bytes.
		targetStorageKey := writepath.NewStorageKey(key)
		sourceLoc, err := r.CopyToReplica(ctx, key, targetStorageKey, existingCopies, target)
		if err != nil {
			r.log.WarnContext(ctx, "failed to copy object data",
				"key", key, "target", target, "error", err)
			// A copy can fail after its bytes reached the target, and no row
			// will ever record them. The path is this attempt's own, so
			// removing it cannot touch anything else.
			r.CleanupOrphan(ctx, target, key, targetStorageKey, sizeEstimate)
			telemetry.ReplicationErrorsTotal.Inc()
			exclusion[target] = true
			out.CopyErrors++
			continue
		}
		source, transferredSize := sourceLoc.BackendName, sourceLoc.SizeBytes

		// Checked before the row exists, so a copy that disagrees with its
		// source never counts toward the replication factor.
		if !r.admitVerifiedReplica(ctx, key, target, targetStorageKey, sourceLoc, &out) {
			exclusion[target] = true
			continue
		}

		recordedSize, inserted, err := r.store.RecordReplica(ctx, &core.ReplicaInsert{
			ObjectKey:     key,
			TargetBackend: target,
			SourceBackend: source,
			StorageKey:    targetStorageKey,
		})
		if err != nil {
			r.log.ErrorContext(ctx, "failed to record replica",
				"key", key, "target", target, "error", err)
			r.CleanupOrphan(ctx, target, key, targetStorageKey, transferredSize)
			telemetry.ReplicationErrorsTotal.Inc()
			exclusion[target] = true
			out.RecordErrors++
			continue
		}

		if !inserted {
			// The source copy was deleted or overwritten during replication,
			// or the target ran out of room while the bytes were in flight.
			// Either way no row was written, so the copy on the backend is an
			// orphan and this target is out of the running.
			r.log.InfoContext(ctx, "replica not recorded, cleaning up orphan",
				"key", key, "target", target)
			r.CleanupOrphan(ctx, target, key, targetStorageKey, transferredSize)
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

		// Charged at the size the metadata commit settled on, not the size
		// that crossed the wire. StreamCopy admitted the transfer against
		// both backends' limits; it does not account for it.
		r.ops.Acct().Egress(s3op.GetObject, source, recordedSize)
		r.ops.Acct().Ingress(s3op.PutObject, target, recordedSize)

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

// reportObjectOutcome logs one warning for an object that did not reach the
// replication factor. Outcomes with no failures log nothing.
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
		"verify_mismatch", o.VerifyMismatch,
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

// FindReplicaTarget names the next backend to try for a replication copy,
// using the same routing strategy as normal writes. Returns an empty string
// when no candidate is left. Capacity is settled later by the conditional
// insert that records the copy, so a target named here can still decline.
func (r *Replicator) FindReplicaTarget(ctx context.Context, key string, size int64, exclusion map[string]bool) string {
	ranked := r.placement.RankReplicaTargets(size, exclusion)
	if len(ranked) == 0 {
		return ""
	}
	return ranked[0]
}

// CopyToReplica reads the object from an existing copy and writes it to the
// target backend at targetStorageKey, trying each existing copy in turn. It
// returns the source row that answered; its SizeBytes is the bytes
// transferred, and its stored-form columns describe the target too, since
// StreamCopy moves bytes verbatim. The caller's slice is not reordered.
func (r *Replicator) CopyToReplica(ctx context.Context, key, targetStorageKey string, copies []core.ObjectLocation, target string) (*core.ObjectLocation, error) {
	targetBackend, err := r.ops.GetBackend(target)
	if err != nil {
		return nil, err
	}

	// Prefer healthy sources. Sort a clone, since ReplicateObject reuses the
	// caller's slice across attempts.
	ordered := slices.Clone(copies)
	slices.SortStableFunc(ordered, func(a, b core.ObjectLocation) int {
		return cmpHealthFirst(r.IsBackendHealthy(a.BackendName), r.IsBackendHealthy(b.BackendName))
	})

	// Drop sources whose breaker is open so a copy never starts against a
	// known-down backend. Fall back to the full set when no source is
	// healthy, so the object still gets an attempt.
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
			return nil, ctx.Err()
		}
		source, terminal, err := r.tryCopyFrom(ctx, key, targetStorageKey, target, targetBackend, &candidates[i])
		if terminal {
			return source, err
		}
	}

	return nil, fmt.Errorf("all source copies failed for key %s", key)
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

// tryCopyFrom attempts a stream-copy from one source location to the target.
// It returns terminal=true on success, or on a *backend.CopyError with
// CopyPhaseWrite, where no other source could help. Any other error returns
// terminal=false so the caller tries the next source.
func (r *Replicator) tryCopyFrom(ctx context.Context, key, targetStorageKey, target string, targetBackend backend.ObjectBackend, loc *core.ObjectLocation) (*core.ObjectLocation, bool, error) {
	srcBackend, ok := r.ops.Backends()[loc.BackendName]
	if !ok {
		return nil, false, nil
	}
	src := backend.CopyEndpoint{Name: loc.BackendName, Backend: srcBackend}
	dst := backend.CopyEndpoint{Name: target, Backend: targetBackend}
	// StreamCopy tags a usage-limit refusal with the leg that had no headroom,
	// so a source out of egress falls through to the next candidate and a
	// full destination is terminal, as with an I/O failure.
	_, err := r.ops.StreamCopy(ctx, src, dst,
		core.StoragePath(key, loc.StorageKey), targetStorageKey, loc.SizeBytes)
	if err == nil {
		return loc, true, nil
	}
	// Write failures won't improve with a different source - fail immediately.
	if backend.IsCopyPhase(err, backend.CopyPhaseWrite) {
		return nil, true, fmt.Errorf("failed to write to target %s: %w", target, err)
	}
	r.log.WarnContext(ctx, "source read failed, trying next copy",
		"key", key, "source", loc.BackendName, "error", err)
	if backend.IsNotFound(err) {
		r.pruneStaleSource(ctx, key, loc.BackendName)
	}
	return nil, false, nil
}

// pruneStaleSource removes a source-side ObjectLocation row when the
// backend reported the object missing. Logging-only on DB failure: a
// stuck stale row is preferable to aborting the replication pass.
func (r *Replicator) pruneStaleSource(ctx context.Context, key, backendName string) {
	_, delErr := r.store.DeleteObjectLocation(ctx, key, backendName)
	if delErr != nil {
		r.log.WarnContext(ctx, "failed to remove stale metadata",
			"key", key, "backend", backendName, "error", delErr)
		return
	}
	r.log.InfoContext(ctx, "removed stale metadata entry",
		"key", key, "backend", backendName)
}

// CleanupOrphan deletes the bytes this replication attempt wrote when the DB
// record was not created (e.g. source was deleted during replication). It
// deletes only the path the attempt wrote, so an existing copy on the target
// is kept.
func (r *Replicator) CleanupOrphan(ctx context.Context, backendName, key, storageKey string, sizeBytes int64) {
	be, ok := r.ops.Backends()[backendName]
	if !ok {
		return
	}
	r.placement.DeleteOrEnqueue(ctx, be, &core.CleanupRequest{
		BackendName: backendName,
		ObjectKey:   key,
		StorageKey:  storageKey,
		Reason:      "replication_orphan",
		SizeBytes:   sizeBytes,
	})
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

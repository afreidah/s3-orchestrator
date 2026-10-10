// -------------------------------------------------------------------------------
// Drainer - Background Backend Drain Worker
//
// Author: Alex Freidah
//
// Moves every object off a backend that has a drain record in progress. The
// record is the drain: an operator starts one by writing it and cancels one by
// clearing it, admission refuses the backend for as long as it exists, and this
// worker picks it up on its next tick, so a restart resumes a drain rather than
// forgetting it. A drain finishes only when the store confirms nothing is left
// on the backend, and a hard error leaves the record failed rather than
// re-opening the backend to writes.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// drainPageSize is how many of a backend's objects one page moves.
const drainPageSize = 100

// errListDrainObjects marks a failure to list the draining backend's objects,
// which fails the drain unless the database itself is unavailable.
var errListDrainObjects = errors.New("list objects")

// -------------------------------------------------------------------------
// DRAINER TYPE
// -------------------------------------------------------------------------

// DrainerStore is the persistence surface the drainer needs: the backend's
// objects and their copies, the drain records it advances, and the conditional
// removal that drops a drained copy only while another copy remains.
type DrainerStore interface {
	core.ObjectStore
	core.DrainStore
	RemoveExcessCopy(ctx context.Context, key, backendName string, factor int) (core.RemovedCopy, error)
}

// Drainer moves objects off backends being drained.
type Drainer struct {
	log          *slog.Logger
	ops          Ops
	placement    Placement
	store        DrainerStore
	abortUploads func(ctx context.Context, backendName string)
	onRecords    func(drains []core.BackendDrain)
}

// DrainerDeps groups the drainer's constructor dependencies. AbortUploads
// aborts a backend's open multipart uploads. OnRecords is optional and receives
// the drain records each pass reads, so whatever caches them stays current.
type DrainerDeps struct {
	Ops          Ops
	Placement    Placement
	Store        DrainerStore
	AbortUploads func(ctx context.Context, backendName string)
	OnRecords    func(drains []core.BackendDrain)
}

// NewDrainer creates a Drainer with the given dependencies.
func NewDrainer(deps DrainerDeps) *Drainer {
	must.NotNil("Ops", deps.Ops)
	must.NotNil("Placement", deps.Placement)
	must.NotNil("Store", deps.Store)
	must.NotNil("AbortUploads", deps.AbortUploads)
	return &Drainer{
		ops:          deps.Ops,
		placement:    deps.Placement,
		store:        deps.Store,
		abortUploads: deps.AbortUploads,
		onRecords:    deps.OnRecords,
		log:          slog.Default().With(logfmt.Component("drainer")),
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// DrainSummary is the outcome of one drain pass: the per-object tally across
// every backend the pass worked on, plus how many of those drains it finished.
type DrainSummary struct {
	batch.Summary
	Completed int
}

// Drain walks every drain in progress once, moving what it can and finishing
// the drains whose backend ends up empty. observer, when non-nil, receives a
// start and end step per object.
func (d *Drainer) Drain(ctx context.Context, observer progress.Observer) (DrainSummary, error) {
	return runOpsCycle(ctx, "Drain", "drain", func(ctx context.Context) (DrainSummary, error) {
		drains, err := d.store.ListDrains(ctx)
		if err != nil {
			return DrainSummary{}, fmt.Errorf("list drains: %w", err)
		}
		if d.onRecords != nil {
			d.onRecords(drains)
		}
		active := inProgress(drains)
		telemetry.DrainActive.Set(float64(len(active)))

		var out DrainSummary
		for i := range active {
			sum, completed, err := d.drainBackend(ctx, &active[i], observer)
			out.Summary = out.Plus(sum)
			if completed {
				out.Completed++
			}
			if err != nil {
				return out, err
			}
		}
		return out, nil
	})
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// inProgress returns the drains still in progress.
func inProgress(drains []core.BackendDrain) []core.BackendDrain {
	var active []core.BackendDrain
	for i := range drains {
		if drains[i].State == core.DrainStateDraining {
			active = append(active, drains[i])
		}
	}
	return active
}

// drainBackend walks one backend's objects smallest first and moves each off.
// An object that fails to move is passed over, so the pass reaches the end of
// the listing and the store decides whether the drain is finished; it refuses
// while any object remains, and the next tick retries what is left. Reports
// whether the drain finished. Returns an error only for a store failure the
// next tick should retry; a failure that ends the drain is recorded on it.
func (d *Drainer) drainBackend(ctx context.Context, rec *core.BackendDrain, observer progress.Observer) (batch.Summary, bool, error) {
	name := rec.BackendName
	src, err := d.ops.GetBackend(name)
	if err != nil {
		return batch.Summary{}, false, d.fail(ctx, rec, 0, fmt.Errorf("backend not configured: %w", err))
	}
	d.abortUploads(ctx, name)

	pager := batch.Pager[core.ObjectLocation, core.SizeCursor]{
		PageSize: batch.FixedPage(drainPageSize),
		List: func(ctx context.Context, limit int, after core.SizeCursor) ([]core.ObjectLocation, error) {
			page, err := d.store.ListObjectsByBackend(ctx, name, limit, after)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", errListDrainObjects, err)
			}
			return page, nil
		},
		CursorOf: func(obj core.ObjectLocation) core.SizeCursor {
			return core.SizeCursor{SizeBytes: obj.SizeBytes, ObjectKey: obj.ObjectKey}
		},
		// A cancel or a failure recorded elsewhere stops the pass here.
		BeforePage: func(ctx context.Context) (bool, error) {
			return d.stillDraining(ctx, name)
		},
	}
	var total batch.Summary
	stop, err := pager.Walk(ctx, func(ctx context.Context, page []core.ObjectLocation) (batch.Step, error) {
		sum, err := d.movePage(ctx, src, name, page, observer)
		if err != nil {
			return batch.Step{}, err
		}
		total = total.Plus(sum)
		if sum.Succeeded > 0 {
			if err := d.store.AddDrainedObjects(ctx, name, int64(sum.Succeeded)); err != nil {
				return batch.Step{}, err
			}
		}
		return batch.Step{Progress: sum.Succeeded}, nil
	})
	switch stop {
	case batch.Exhausted:
		done, err := d.complete(ctx, rec, total.Succeeded)
		return total, done, err
	case batch.Errored:
		if errors.Is(err, errListDrainObjects) && !errors.Is(err, core.ErrDBUnavailable) {
			return total, false, d.fail(ctx, rec, total.Succeeded, err)
		}
		return total, false, err
	default:
		return total, false, nil
	}
}

// movePage moves one page of objects off the backend, one at a time. The
// page's copies are looked up once, so an object another backend already holds
// is dropped without a per-object lookup. A failed lookup ends the pass for the
// next tick to retry.
func (d *Drainer) movePage(ctx context.Context, src backend.ObjectBackend, srcName string, page []core.ObjectLocation, observer progress.Observer) (batch.Summary, error) {
	keys := make([]string, len(page))
	for i := range page {
		keys[i] = page[i].ObjectKey
	}
	copies, err := d.store.GetObjectBackendsForKeys(ctx, keys)
	if err != nil {
		return batch.Summary{}, fmt.Errorf("look up copies: %w", err)
	}
	runner := batch.Runner[core.ObjectLocation]{
		Name:        "drain",
		Concurrency: 1,
		Observer:    observer,
		Key:         func(obj core.ObjectLocation) string { return obj.ObjectKey },
	}
	return runner.Run(ctx, page, func(ctx context.Context, obj core.ObjectLocation) batch.ItemResult {
		var res batch.ItemResult // zero value (batch.ItemSkipped) when admission blocks the move
		WithAdmission(ctx, d.ops, WorkerNameDrainer, func() {
			if d.drainOne(ctx, src, srcName, &obj, copies[obj.ObjectKey]) {
				telemetry.DrainObjectsMoved.Inc()
				telemetry.DrainBytesMoved.Add(float64(obj.SizeBytes))
				res = batch.ItemResult{Outcome: batch.ItemSucceeded}
			} else {
				res = batch.ItemResult{Outcome: batch.ItemFailed}
			}
		})
		return res
	}), nil
}

// stillDraining reports whether the backend's record is still a drain in
// progress.
func (d *Drainer) stillDraining(ctx context.Context, name string) (bool, error) {
	drains, err := d.store.ListDrains(ctx)
	if err != nil {
		return false, err
	}
	for i := range drains {
		if drains[i].BackendName == name {
			return drains[i].State == core.DrainStateDraining, nil
		}
	}
	return false, nil
}

// complete asks the store to mark the drain finished once the pass has walked
// the whole listing. The store declines while any managed object remains, a
// write admitted before the drain is still uploading, or a multipart upload is
// open, and the next tick asks again.
func (d *Drainer) complete(ctx context.Context, rec *core.BackendDrain, movedThisPass int) (bool, error) {
	done, err := d.store.CompleteDrain(ctx, rec.BackendName)
	if err != nil || !done {
		return false, err
	}
	moved := rec.ObjectsMoved + int64(movedThisPass)
	audit.Log(ctx, "storage.DrainComplete",
		slog.String("backend", rec.BackendName),
		slog.Int64("objects_moved", moved),
	)
	event.Publish(event.BackendDrainCompleted, rec.BackendName, map[string]any{
		"backend":       rec.BackendName,
		"objects_moved": moved,
	})
	d.log.InfoContext(ctx, "backend drain complete", "backend", rec.BackendName, "objects_moved", moved)
	return true, nil
}

// fail records why the drain stopped. The record stays, so the backend keeps
// refusing writes until an operator retries or clears the drain.
func (d *Drainer) fail(ctx context.Context, rec *core.BackendDrain, movedThisPass int, cause error) error {
	if err := d.store.MarkDrainFailed(ctx, rec.BackendName, cause.Error()); err != nil {
		return err
	}
	moved := rec.ObjectsMoved + int64(movedThisPass)
	event.Publish(event.BackendDrainFailed, rec.BackendName, map[string]any{
		"backend":       rec.BackendName,
		"objects_moved": moved,
		"error":         cause.Error(),
	})
	d.log.ErrorContext(ctx, "backend drain failed", "backend", rec.BackendName, logfmt.Err(cause))
	return nil
}

// drainOne moves a single object off the draining backend. backends are the
// backends the page lookup found holding a copy. When another one does, the
// draining copy is dropped and nothing is transferred. Returns true on success.
func (d *Drainer) drainOne(ctx context.Context, src backend.ObjectBackend, srcName string, obj *core.ObjectLocation, backends []string) bool {
	if other := otherCopyBackend(backends, srcName); other != "" {
		return d.dropDrainedCopy(ctx, src, srcName, obj, other)
	}
	return d.moveOff(ctx, src, srcName, obj)
}

// otherCopyBackend returns a backend other than srcName that holds a copy, or
// "" when the draining backend holds the only one.
func otherCopyBackend(backends []string, srcName string) string {
	for _, b := range backends {
		if b != srcName {
			return b
		}
	}
	return ""
}

// dropDrainedCopy removes the draining backend's copy of an object another
// backend also held when the page was looked up: the row first, then the
// bytes. The removal re-reads the copies under the key lock and removes this
// one only while another remains, so a lookup that went stale during the page
// cannot cost the last copy. If the other copy is gone, the object is moved
// off instead. A copy that holds the only usable encryption key is kept.
func (d *Drainer) dropDrainedCopy(ctx context.Context, src backend.ObjectBackend, srcName string, obj *core.ObjectLocation, otherBackend string) bool {
	removed, err := d.store.RemoveExcessCopy(ctx, obj.ObjectKey, srcName, 1)
	switch {
	case errors.Is(err, core.ErrCopyHoldsOnlyDEK):
		d.log.WarnContext(ctx, "not dropping drained copy: it holds the only usable encryption key",
			"key", obj.ObjectKey, "backend", srcName)
		return false
	case err != nil:
		d.log.WarnContext(ctx, "failed to delete source location",
			"key", obj.ObjectKey, "backend", srcName, logfmt.Err(err))
		return false
	case !removed.Removed:
		return d.moveOff(ctx, src, srcName, obj)
	}
	d.placement.DeleteOrEnqueue(ctx, src, &core.CleanupRequest{
		BackendName: srcName,
		ObjectKey:   obj.ObjectKey,
		StorageKey:  removed.StorageKey,
		Reason:      "drain_source_delete",
		SizeBytes:   removed.SizeBytes,
	})
	audit.Log(ctx, "storage.DrainRemoveReplica",
		slog.String("key", obj.ObjectKey),
		slog.String("removed_from", srcName),
		slog.String("exists_on", otherBackend),
	)
	return true
}

// moveOff copies an object the draining backend holds the only copy of to
// another backend, moves its row there, and deletes the source bytes, all
// through the coordinator's MoveObject.
func (d *Drainer) moveOff(ctx context.Context, src backend.ObjectBackend, srcName string, obj *core.ObjectLocation) bool {
	destName, dest, ok := d.pickDestination(ctx, srcName, obj)
	if !ok {
		return false
	}
	movedSize, err := d.placement.MoveObject(ctx, &writepath.MoveRequest{
		Key:            obj.ObjectKey,
		SizeBytes:      obj.SizeBytes,
		SrcBackend:     src,
		SrcName:        srcName,
		DestBackend:    dest,
		DestName:       destName,
		SrcStorageKey:  core.StoragePath(obj.ObjectKey, obj.StorageKey),
		DestStorageKey: writepath.NewStorageKey(obj.ObjectKey),
		Reasons:        writepath.DrainMoveReasons,
	})
	if err != nil {
		if !errors.Is(err, writepath.ErrMoveStale) {
			d.log.WarnContext(ctx, "drain move failed",
				"key", obj.ObjectKey, "src_backend", srcName, "dst_backend", destName, logfmt.Err(err))
		}
		return false
	}
	audit.Log(ctx, "storage.DrainMove",
		slog.String("key", obj.ObjectKey),
		slog.String("src_backend", srcName),
		slog.String("dst_backend", destName),
		slog.Int64("size", movedSize),
	)
	return true
}

// pickDestination chooses the emptiest backend, other than the source and any
// backend being drained, with room for the object. It reads the same
// in-memory view a client write is ranked against, so a drain and a write do
// not disagree about where there is room.
func (d *Drainer) pickDestination(ctx context.Context, srcName string, obj *core.ObjectLocation) (string, backend.ObjectBackend, bool) {
	candidates := make([]string, 0, len(d.ops.BackendOrder()))
	for _, name := range d.ops.BackendOrder() {
		if name != srcName && !d.ops.IsDraining(name) {
			candidates = append(candidates, name)
		}
	}
	quota := d.ops.Quota()
	for _, name := range quota.RankByUtilization(candidates) {
		if quota.Available(name) < obj.SizeBytes {
			continue
		}
		dest, err := d.ops.GetBackend(name)
		if err != nil {
			d.log.ErrorContext(ctx, "destination backend not found", "backend", name)
			return "", nil, false
		}
		return name, dest, true
	}
	d.log.WarnContext(ctx, "no destination backend available",
		"key", obj.ObjectKey, "size_bytes", obj.SizeBytes)
	return "", nil, false
}

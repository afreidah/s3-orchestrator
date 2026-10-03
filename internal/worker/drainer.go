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
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// drainPageSize is how many of a backend's objects one page moves.
const drainPageSize = 100

// -------------------------------------------------------------------------
// DRAINER TYPE
// -------------------------------------------------------------------------

// DrainerStore is the persistence surface the drainer needs: the backend's
// objects and their copies, and the drain records it advances.
type DrainerStore interface {
	core.ObjectStore
	core.DrainStore
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
	WorkSummary
	Completed int
}

// Drain works every drain in progress until its backend is empty, its record
// is cleared or fails, or a page moves nothing. observer, when non-nil,
// receives a start and end step per object.
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
			out.WorkSummary = out.Plus(sum)
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

// drainBackend moves one backend's objects off a page at a time. Reports
// whether the drain finished. Returns an error only for a store failure the
// next tick should retry; a failure that ends the drain is recorded on it.
func (d *Drainer) drainBackend(ctx context.Context, rec *core.BackendDrain, observer progress.Observer) (WorkSummary, bool, error) {
	name := rec.BackendName
	src, err := d.ops.GetBackend(name)
	if err != nil {
		return WorkSummary{}, false, d.fail(ctx, rec, 0, fmt.Errorf("backend not configured: %w", err))
	}
	d.abortUploads(ctx, name)

	var total WorkSummary
	for ctx.Err() == nil {
		page, done, err := d.nextPage(ctx, rec, total.Succeeded)
		if err != nil || page == nil {
			return total, done, err
		}
		sum := d.movePage(ctx, src, name, page, observer)
		total = total.Plus(sum)
		if sum.Succeeded == 0 {
			// Every object on the page failed or was refused admission. They
			// are listed first again, so carrying on would retry the same
			// page; the next tick tries again instead.
			d.log.WarnContext(ctx, "drain page moved nothing, retrying next tick",
				"backend", name, "page", len(page), "failed", sum.Failed, "skipped", sum.Skipped)
			return total, false, nil
		}
		if err := d.store.AddDrainedObjects(ctx, name, int64(sum.Succeeded)); err != nil {
			return total, false, err
		}
	}
	return total, false, nil
}

// nextPage returns the backend's next page of objects to move. A nil page ends
// the pass: the record was cleared or failed elsewhere, the drain failed here,
// or the backend listed empty, in which case done reports whether the store
// let the drain finish.
func (d *Drainer) nextPage(ctx context.Context, rec *core.BackendDrain, movedThisPass int) ([]core.ObjectLocation, bool, error) {
	draining, err := d.stillDraining(ctx, rec.BackendName)
	if err != nil || !draining {
		return nil, false, err
	}
	page, err := d.store.ListObjectsByBackend(ctx, rec.BackendName, drainPageSize)
	if err != nil {
		if errors.Is(err, core.ErrDBUnavailable) {
			return nil, false, err
		}
		return nil, false, d.fail(ctx, rec, movedThisPass, fmt.Errorf("list objects: %w", err))
	}
	if len(page) == 0 {
		done, err := d.complete(ctx, rec, movedThisPass)
		return nil, done, err
	}
	return page, false, nil
}

// movePage moves one page of objects off the backend, one at a time.
func (d *Drainer) movePage(ctx context.Context, src backend.ObjectBackend, srcName string, page []core.ObjectLocation, observer progress.Observer) WorkSummary {
	runner := BatchRunner[core.ObjectLocation]{
		Name:        "drain",
		Concurrency: 1,
		Observer:    observer,
		Key:         func(obj core.ObjectLocation) string { return obj.ObjectKey },
	}
	return runner.Run(ctx, page, func(ctx context.Context, obj core.ObjectLocation) ItemResult {
		var res ItemResult // zero value (ItemSkipped) when admission blocks the move
		WithAdmission(ctx, d.ops, WorkerNameDrainer, func() {
			if d.drainOne(ctx, src, srcName, &obj) {
				telemetry.DrainObjectsMoved.Inc()
				telemetry.DrainBytesMoved.Add(float64(obj.SizeBytes))
				res = ItemResult{Outcome: ItemSucceeded}
			} else {
				res = ItemResult{Outcome: ItemFailed}
			}
		})
		return res
	})
}

// stillDraining reports whether the backend's record is still a drain in
// progress. Checked before every page, which is how a cancel or a failure
// recorded elsewhere stops the pass.
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

// complete asks the store to mark the drain finished once the backend lists
// empty. The store declines while a write admitted before the drain is still
// uploading or a multipart upload is open, and the next tick asks again.
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

// drainOne moves a single object off the draining backend. When another
// backend already holds a copy, the draining copy is dropped and nothing is
// transferred. Returns true on success.
func (d *Drainer) drainOne(ctx context.Context, src backend.ObjectBackend, srcName string, obj *core.ObjectLocation) bool {
	locations, err := d.store.GetAllObjectLocations(ctx, obj.ObjectKey)
	if err != nil {
		d.log.WarnContext(ctx, "failed to look up object locations", "key", obj.ObjectKey, logfmt.Err(err))
		return false
	}
	if other := otherCopyBackend(locations, srcName); other != "" {
		return d.dropDrainedCopy(ctx, src, srcName, obj, other)
	}
	return d.moveOff(ctx, src, srcName, obj)
}

// otherCopyBackend returns a backend other than srcName that holds a copy, or
// "" when the draining backend holds the only one.
func otherCopyBackend(locations []core.ObjectLocation, srcName string) string {
	for i := range locations {
		if locations[i].BackendName != srcName {
			return locations[i].BackendName
		}
	}
	return ""
}

// dropDrainedCopy removes the draining backend's copy of an object another
// backend also holds: the row first, then the bytes.
func (d *Drainer) dropDrainedCopy(ctx context.Context, src backend.ObjectBackend, srcName string, obj *core.ObjectLocation, otherBackend string) bool {
	if _, err := d.store.DeleteObjectLocation(ctx, obj.ObjectKey, srcName); err != nil {
		d.log.WarnContext(ctx, "failed to delete source location",
			"key", obj.ObjectKey, "backend", srcName, logfmt.Err(err))
		return false
	}
	d.placement.DeleteOrEnqueue(ctx, src, srcName, obj.ObjectKey, "drain_source_delete", obj.SizeBytes)
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
		Key:         obj.ObjectKey,
		SizeBytes:   obj.SizeBytes,
		SrcBackend:  src,
		SrcName:     srcName,
		DestBackend: dest,
		DestName:    destName,
		Reasons:     writepath.DrainMoveReasons,
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

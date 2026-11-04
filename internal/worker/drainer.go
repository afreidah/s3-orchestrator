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
	"slices"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/cycle"
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
// objects and their copies, including the conditional removal that drops a
// drained copy only while another copy remains, and the backend state records
// it advances.
type DrainerStore interface {
	core.ObjectStore
	core.BackendStateStore
}

// Drainer moves objects off backends being drained.
type Drainer struct {
	d   DrainerDeps
	log *slog.Logger
}

// DrainerDeps groups the drainer's constructor dependencies. AbortUploads
// aborts a backend's open multipart uploads. OnRecords is optional and receives
// the backend state records each pass reads, so whatever caches them stays
// current.
type DrainerDeps struct {
	Fleet
	Store        DrainerStore
	AbortUploads func(ctx context.Context, backendName string)
	OnRecords    func(records []core.BackendStateRecord)
}

// NewDrainer creates a Drainer with the given dependencies.
//
//nolint:gocritic // hugeParam: the Deps are copied once, at construction.
func NewDrainer(d DrainerDeps) *Drainer {
	d.mustBeWired()
	must.NotNil("d.Store", d.Store)
	must.NotNil("d.AbortUploads", d.AbortUploads)
	return &Drainer{d: d, log: logfmt.Logger(string(audit.WorkloadDrain))}
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

// AuditAttrs adds the drains finished to the tally's attributes.
func (s DrainSummary) AuditAttrs() []slog.Attr {
	return append(s.Summary.AuditAttrs(), slog.Int("drains_completed", s.Completed))
}

// Drain walks every drain in progress once, moving what it can and finishing
// the drains whose backend ends up empty. observer, when non-nil, receives a
// start and end step per object.
func (dr *Drainer) Drain(ctx context.Context, observer progress.Observer) (DrainSummary, error) {
	pass := cycle.Pass{Workload: audit.WorkloadDrain, Refresh: dr.d.Metrics}
	return cycle.Run(ctx, pass, func(ctx context.Context) (DrainSummary, error) {
		records, err := dr.d.Store.ListBackendStates(ctx)
		if err != nil {
			return DrainSummary{}, fmt.Errorf("list backend states: %w", err)
		}
		if dr.d.OnRecords != nil {
			dr.d.OnRecords(records)
		}
		active := inProgress(records)
		telemetry.DrainActive.Set(float64(len(active)))

		var out DrainSummary
		for i := range active {
			sum, completed, err := dr.drainBackend(ctx, &active[i], observer)
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

// inProgress returns the records whose drain is still in progress.
func inProgress(records []core.BackendStateRecord) []core.BackendStateRecord {
	var active []core.BackendStateRecord
	for i := range records {
		if records[i].Flags.Any(core.FlagDraining) {
			active = append(active, records[i])
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
func (dr *Drainer) drainBackend(ctx context.Context, rec *core.BackendStateRecord, observer progress.Observer) (batch.Summary, bool, error) {
	name := rec.BackendName
	if _, err := dr.d.Ops.GetBackend(name); err != nil {
		return batch.Summary{}, false, dr.fail(ctx, rec, 0, fmt.Errorf("backend not configured: %w", err))
	}
	dr.d.AbortUploads(ctx, name)

	pager := batch.Pager[core.ObjectLocation, core.SizeCursor]{
		PageSize: batch.FixedPage(drainPageSize),
		List: func(ctx context.Context, limit int, after core.SizeCursor) ([]core.ObjectLocation, error) {
			page, err := dr.d.Store.ListObjectsByBackend(ctx, name, limit, after)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", errListDrainObjects, err)
			}
			return page, nil
		},
		CursorOf: core.SizeCursorOf,
		// A cancel or a failure recorded elsewhere stops the pass here.
		BeforePage: func(ctx context.Context) (bool, error) {
			return dr.stillDraining(ctx, name)
		},
	}
	var total batch.Summary
	stop, err := pager.Walk(ctx, func(ctx context.Context, page []core.ObjectLocation) (batch.Step, error) {
		sum, err := dr.movePage(ctx, name, page, observer)
		if err != nil {
			return batch.Step{}, err
		}
		total = total.Plus(sum)
		if sum.Succeeded > 0 {
			if err := dr.d.Store.AddDrainedObjects(ctx, name, int64(sum.Succeeded)); err != nil {
				return batch.Step{}, err
			}
		}
		return batch.Step{Progress: sum.Succeeded}, nil
	})
	switch stop {
	case batch.Exhausted:
		done, err := dr.complete(ctx, rec, total.Succeeded)
		return total, done, err
	case batch.Errored:
		if errors.Is(err, errListDrainObjects) && !errors.Is(err, core.ErrDBUnavailable) {
			return total, false, dr.fail(ctx, rec, total.Succeeded, err)
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
func (dr *Drainer) movePage(ctx context.Context, srcName string, page []core.ObjectLocation, observer progress.Observer) (batch.Summary, error) {
	copies, err := dr.d.Store.GetObjectBackendsForKeys(ctx, core.KeysOf(page))
	if err != nil {
		return batch.Summary{}, fmt.Errorf("look up copies: %w", err)
	}
	runner := batch.Runner[core.ObjectLocation]{
		Concurrency: 1,
		Admission:   dr.d.Ops,
		Observer:    observer,
		Key:         func(obj core.ObjectLocation) string { return obj.ObjectKey },
	}
	return runner.Run(ctx, page, func(ctx context.Context, obj core.ObjectLocation) batch.ItemResult {
		if !dr.drainOne(ctx, srcName, &obj, copies[obj.ObjectKey]) {
			return batch.Failed()
		}
		telemetry.DrainObjectsMoved.Inc()
		telemetry.DrainBytesMoved.Add(float64(obj.SizeBytes))
		return batch.Succeeded()
	}), nil
}

// stillDraining reports whether the backend's record is still a drain in
// progress.
func (dr *Drainer) stillDraining(ctx context.Context, name string) (bool, error) {
	records, err := dr.d.Store.ListBackendStates(ctx)
	if err != nil {
		return false, err
	}
	for i := range records {
		if records[i].BackendName == name {
			return records[i].Flags.Any(core.FlagDraining), nil
		}
	}
	return false, nil
}

// complete asks the store to mark the drain finished once the pass has walked
// the whole listing. The store declines while any managed object remains, a
// write admitted before the drain is still uploading, or a multipart upload is
// open, and the next tick asks again.
func (dr *Drainer) complete(ctx context.Context, rec *core.BackendStateRecord, movedThisPass int) (bool, error) {
	done, err := dr.d.Store.CompleteDrain(ctx, rec.BackendName)
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
	dr.log.InfoContext(ctx, "backend drain complete", "backend", rec.BackendName, "objects_moved", moved)
	return true, nil
}

// fail records why the drain stopped. The record stays, so the backend keeps
// refusing writes until an operator retries or clears the drain.
func (dr *Drainer) fail(ctx context.Context, rec *core.BackendStateRecord, movedThisPass int, cause error) error {
	if err := dr.d.Store.MarkDrainFailed(ctx, rec.BackendName, cause.Error()); err != nil {
		return err
	}
	moved := rec.ObjectsMoved + int64(movedThisPass)
	event.Publish(event.BackendDrainFailed, rec.BackendName, map[string]any{
		"backend":       rec.BackendName,
		"objects_moved": moved,
		"error":         cause.Error(),
	})
	dr.log.ErrorContext(ctx, "backend drain failed", "backend", rec.BackendName, "error", cause)
	return nil
}

// drainOne moves a single object off the draining backend. backends are the
// backends the page lookup found holding a copy. When another one does, the
// draining copy is dropped and nothing is transferred. Returns true on success.
func (dr *Drainer) drainOne(ctx context.Context, srcName string, obj *core.ObjectLocation, backends []string) bool {
	if other := otherCopyBackend(backends, srcName); other != "" {
		return dr.dropDrainedCopy(ctx, srcName, obj, other)
	}
	return dr.moveOff(ctx, srcName, obj)
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
// backend also held when the page was looked up: the row goes and its bytes are
// queued in one transaction, then deleted. The removal re-reads the copies
// under the key lock and removes this
// one only while another remains, so a lookup that went stale during the page
// cannot cost the last copy. If the other copy is gone, the object is moved
// off instead. A copy that holds the only usable encryption key is kept, and
// one a client overwrote since the page was read is left for the next pass.
func (dr *Drainer) dropDrainedCopy(ctx context.Context, srcName string, obj *core.ObjectLocation, otherBackend string) bool {
	err := dr.d.Placement.DropCopy(ctx, obj, 1, writepath.DrainMove.Reasons.SourceDelete)
	switch {
	case errors.Is(err, core.ErrCopyNotExcess):
		return dr.moveOff(ctx, srcName, obj)
	case errors.Is(err, core.ErrCopyChanged):
		return false
	case errors.Is(err, core.ErrCopyHoldsOnlyDEK):
		dr.log.WarnContext(ctx, "not dropping drained copy: it holds the only usable encryption key",
			"key", obj.ObjectKey, "backend", srcName)
		return false
	case err != nil:
		dr.log.WarnContext(ctx, "failed to delete source location",
			"key", obj.ObjectKey, "backend", srcName, "error", err)
		return false
	}
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
func (dr *Drainer) moveOff(ctx context.Context, srcName string, obj *core.ObjectLocation) bool {
	destName, ok := dr.pickDestination(ctx, srcName, obj)
	if !ok {
		return false
	}
	movedSize, err := dr.d.Placement.MoveObject(ctx, &writepath.MoveRequest{
		Source:   obj,
		DestName: destName,
		Profile:  writepath.DrainMove,
	})
	if err != nil {
		if !core.PlacementDeclined(err) {
			dr.log.WarnContext(ctx, "drain move failed",
				"key", obj.ObjectKey, "src_backend", srcName, "dst_backend", destName, "error", err)
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

// pickDestination chooses the emptiest write target other than the source with
// room for the object. It reads the same in-memory view a client write is
// ranked against, so a drain and a write do not disagree about where there is
// room or which backends may take new bytes.
func (dr *Drainer) pickDestination(ctx context.Context, srcName string, obj *core.ObjectLocation) (string, bool) {
	candidates := slices.DeleteFunc(dr.d.Ops.WriteTargets(), func(name string) bool { return name == srcName })
	quota := dr.d.Ops.Quota()
	for _, name := range quota.RankByUtilization(candidates) {
		if quota.Available(name) >= obj.SizeBytes {
			return name, true
		}
	}
	dr.log.WarnContext(ctx, "no destination backend available",
		"key", obj.ObjectKey, "size_bytes", obj.SizeBytes)
	return "", false
}

// -------------------------------------------------------------------------------
// Backend State Manager - Drain and Remove Operations
//
// Author: Alex Freidah
//
// The operator side of a backend's state record. A drain is a state in the
// metadata store: starting one writes it, cancelling one clears it, and the
// drainer worker moves the objects off while it is in progress. Remove drops
// every database record for a backend, optionally purging its stored objects,
// and runs synchronously.
// -------------------------------------------------------------------------------

package backendstate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// purgePageSize is how many of a backend's objects one purge page removes.
const purgePageSize = 100

// Runtime is the backend-runtime slice the Manager needs: the fleet and the
// delete primitive a purge uses. Defined here at the consumer so
// *infra.BackendRuntime satisfies it structurally without the drain package
// importing infra.
type Runtime interface {
	Backends() map[string]backend.ObjectBackend
	DeleteMany(ctx context.Context, name string, be backend.ObjectBackend, storageKeys []string) map[string]error
}

// DrainProgress holds the current state of a drain operation. Active is true
// while the drain is in progress; State is empty for a backend with no record.
type DrainProgress struct {
	Active           bool   `json:"active"`
	State            string `json:"state,omitempty"`
	ObjectsRemaining int64  `json:"objects_remaining"`
	BytesRemaining   int64  `json:"bytes_remaining"`
	ObjectsMoved     int64  `json:"objects_moved"`
	Error            string `json:"error,omitempty"`
}

// Manager handles a backend's state: cordoning, draining and removing backends.
//
// states caches every backend's flags from the records, so eligibility filters
// answer on the write path without a query. It is refreshed whenever this
// manager changes a record, on every usage-flush tick, and whenever the drainer
// reads the records. A stale entry only costs a ranking: admission reads the
// records itself.
type Manager struct {
	log     *slog.Logger
	infra   Runtime
	objects core.ObjectStore
	store   core.BackendStateStore
	states  atomic.Pointer[map[string]core.BackendFlags]
}

// transition is one operator change to a backend's state: the store call that
// makes it, the error when it changes nothing, and what is recorded once it
// has. refused is a format taking the backend name, and event is empty when the
// change publishes none.
type transition struct {
	apply   func(ctx context.Context, name string) (bool, error)
	refused string
	audit   string
	event   string
	logMsg  string
}

// New creates a Manager.
func New(infra Runtime, objects core.ObjectStore, store core.BackendStateStore) *Manager {
	must.NotNil("infra", infra)
	must.NotNil("objects", objects)
	must.NotNil("store", store)
	return &Manager{
		infra:   infra,
		objects: objects,
		store:   store,
		log:     slog.Default().With(logfmt.Component("backendstate")),
	}
}

// Flags returns the named backend's cached state flags, which are empty for a
// backend in normal service.
func (d *Manager) Flags(name string) core.BackendFlags {
	states := d.states.Load()
	if states == nil {
		return 0
	}
	return (*states)[name]
}

// Refresh reloads the cached states from the records.
func (d *Manager) Refresh(ctx context.Context) error {
	records, err := d.store.ListBackendStates(ctx)
	if err != nil {
		return fmt.Errorf("list backend states: %w", err)
	}
	d.SetStates(records)
	return nil
}

// SetStates replaces the cached states with the given records and republishes
// the state gauge from them. The drainer hands it the records each pass reads.
func (d *Manager) SetStates(records []core.BackendStateRecord) {
	states := make(map[string]core.BackendFlags, len(records))
	telemetry.BackendState.Reset()
	for i := range records {
		states[records[i].BackendName] = records[i].Flags
		for _, name := range records[i].Flags.Names() {
			telemetry.BackendState.WithLabelValues(records[i].BackendName, name).Set(1)
		}
	}
	d.states.Store(&states)
}

// change applies one transition and, when it changed the record, refreshes
// the cache and records the change.
func (d *Manager) change(ctx context.Context, name string, t *transition) error {
	changed, err := t.apply(ctx, name)
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf(t.refused, name)
	}
	d.refreshAfterChange(ctx)
	audit.Log(ctx, t.audit, slog.String("backend", name))
	if t.event != "" {
		event.Publish(t.event, name, map[string]any{"backend": name})
	}
	d.log.InfoContext(ctx, t.logMsg, "backend", name)
	return nil
}

// requireBackend refuses a backend the runtime does not know, so a typo never
// leaves a state record for a backend that does not exist.
func (d *Manager) requireBackend(name string) error {
	if _, ok := d.infra.Backends()[name]; !ok {
		return fmt.Errorf("backend %q not found", name)
	}
	return nil
}

// -------------------------------------------------------------------------
// CORDON
// -------------------------------------------------------------------------

// Cordon stops new copies landing on the backend. It keeps serving reads, and
// deletes and cleanup still reach it. A cordon is independent of a drain.
func (d *Manager) Cordon(ctx context.Context, name string) error {
	if err := d.requireBackend(name); err != nil {
		return err
	}
	return d.change(ctx, name, &transition{
		apply: func(ctx context.Context, name string) (bool, error) {
			return d.store.SetBackendFlag(ctx, name, core.FlagCordoned)
		},
		refused: "backend %q is already cordoned",
		audit:   "storage.Cordon",
		event:   event.BackendCordoned,
		logMsg:  "backend cordoned",
	})
}

// Uncordon lets new copies land on the backend again. A drain in progress
// carries on.
func (d *Manager) Uncordon(ctx context.Context, name string) error {
	return d.change(ctx, name, &transition{
		apply: func(ctx context.Context, name string) (bool, error) {
			return d.store.ClearBackendFlags(ctx, name, core.FlagCordoned)
		},
		refused: "backend %q is not cordoned",
		audit:   "storage.Uncordon",
		event:   event.BackendUncordoned,
		logMsg:  "backend uncordoned",
	})
}

// -------------------------------------------------------------------------
// DRAIN
// -------------------------------------------------------------------------

// StartDrain records a drain for the backend, or restarts a failed one. From
// that moment admission refuses the backend, and the drainer moves its objects
// off on its next tick. A cordon on the backend is kept.
func (d *Manager) StartDrain(ctx context.Context, name string) error {
	if err := d.requireBackend(name); err != nil {
		return err
	}
	return d.change(ctx, name, &transition{
		apply:   d.store.StartDrain,
		refused: "backend %q is already draining or drained",
		audit:   "storage.DrainStart",
		logMsg:  "backend drain started",
	})
}

// GetDrainProgress returns the state of the backend's drain. Objects and bytes
// remaining are read live while the drain is in progress.
func (d *Manager) GetDrainProgress(ctx context.Context, name string) (*DrainProgress, error) {
	rec, err := d.record(ctx, name)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return &DrainProgress{}, nil
	}
	p := &DrainProgress{
		Active:       rec.Flags.Any(core.FlagDraining),
		State:        rec.Flags.DrainState(),
		ObjectsMoved: rec.ObjectsMoved,
		Error:        rec.LastError,
	}
	if p.Active {
		count, bytes, err := d.store.BackendObjectStats(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("failed to get backend stats: %w", err)
		}
		p.ObjectsRemaining = count
		p.BytesRemaining = bytes
	}
	return p, nil
}

// record reads the backend's state record from the store, bypassing the cache,
// and returns nil when it has none.
func (d *Manager) record(ctx context.Context, name string) (*core.BackendStateRecord, error) {
	records, err := d.store.ListBackendStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("list backend states: %w", err)
	}
	for i := range records {
		if records[i].BackendName == name {
			return &records[i], nil
		}
	}
	return nil, nil
}

// CancelDrain clears the backend's drain flags, in progress, finished or
// failed. A drain in progress stops before its next page; objects already
// moved are not moved back. A cordon on the backend is kept.
func (d *Manager) CancelDrain(ctx context.Context, name string) error {
	return d.change(ctx, name, &transition{
		apply: func(ctx context.Context, name string) (bool, error) {
			return d.store.ClearBackendFlags(ctx, name, core.DrainFlags)
		},
		refused: "backend %q is not draining",
		audit:   "storage.DrainCancel",
		logMsg:  "backend drain cleared",
	})
}

// refreshAfterChange reloads the cached states after this manager changed a
// record. A failure is logged and left to the next refresh: the record is
// already written, and admission reads it directly.
func (d *Manager) refreshAfterChange(ctx context.Context) {
	if err := d.Refresh(ctx); err != nil {
		d.log.WarnContext(ctx, "failed to refresh backend states", logfmt.Err(err))
	}
}

// -------------------------------------------------------------------------
// REMOVE
// -------------------------------------------------------------------------

// RemoveBackend deletes all database records for a backend, its state record
// included. If purge is true and the backend is reachable, also deletes objects
// from the backend's S3 storage. This is destructive and cannot be undone.
// observer, when non-nil, receives a start and end step per object purged.
//
// The drain check reads the record rather than the cache, because a drain
// started or cancelled on another instance may not be cached here yet.
func (d *Manager) RemoveBackend(ctx context.Context, name string, purge bool, observer progress.Observer) error {
	rec, err := d.record(ctx, name)
	if err != nil {
		return err
	}
	if rec != nil && rec.Flags.Any(core.FlagDraining) {
		return fmt.Errorf("backend %q is currently draining, cancel the drain first", name)
	}

	ctx = audit.WithWorkload(audit.WithRequestID(ctx, audit.NewID()), audit.WorkloadPurge)

	if purge {
		if be, ok := d.infra.Backends()[name]; ok {
			d.PurgeBackendObjects(ctx, be, name, observer)
		}
	}

	if err := d.store.DeleteBackendData(ctx, name); err != nil {
		return fmt.Errorf("failed to delete backend data: %w", err)
	}
	if _, err := d.store.ClearBackendState(ctx, name); err != nil {
		return fmt.Errorf("failed to clear backend state: %w", err)
	}
	d.refreshAfterChange(ctx)

	audit.Log(ctx, "storage.RemoveBackend",
		slog.String("backend", name),
		slog.Bool("purge", purge),
	)
	event.Publish(event.BackendRemoved, name, map[string]any{
		"backend": name,
		"purge":   purge,
	})
	d.log.InfoContext(ctx, "backend removed", "backend", name, "purge", purge)

	return nil
}

// PurgeBackendObjects deletes all objects from a backend's S3 storage and their
// metadata rows, walking the backend's listing a page at a time with one
// batched backend delete per page. An object whose row cannot be dropped is
// logged and passed over, so a persistent DB error costs each row one attempt.
func (d *Manager) PurgeBackendObjects(ctx context.Context, be backend.ObjectBackend, name string, observer progress.Observer) {
	pager := batch.Pager[core.ObjectLocation, core.SizeCursor]{
		PageSize: batch.FixedPage(purgePageSize),
		List: func(ctx context.Context, limit int, after core.SizeCursor) ([]core.ObjectLocation, error) {
			return d.objects.ListObjectsByBackend(ctx, name, limit, after)
		},
		CursorOf: func(obj core.ObjectLocation) core.SizeCursor {
			return core.SizeCursor{SizeBytes: obj.SizeBytes, ObjectKey: obj.ObjectKey}
		},
	}
	runner := batch.Runner[core.ObjectLocation]{
		Name:        "purge",
		Concurrency: 1,
		Observer:    observer,
		Key:         func(obj core.ObjectLocation) string { return obj.ObjectKey },
	}
	stop, err := pager.Walk(ctx, func(ctx context.Context, objects []core.ObjectLocation) (batch.Step, error) {
		paths := make(map[string]string, len(objects))
		list := make([]string, len(objects))
		for i := range objects {
			list[i] = core.StoragePath(objects[i].ObjectKey, objects[i].StorageKey)
			paths[objects[i].ObjectKey] = list[i]
		}
		failed := d.infra.DeleteMany(ctx, name, be, list)
		sum := runner.Run(ctx, objects, func(ctx context.Context, obj core.ObjectLocation) batch.ItemResult {
			return d.purgeOneObject(ctx, name, &obj, failed[paths[obj.ObjectKey]])
		})
		return batch.Step{Progress: sum.Succeeded}, nil
	})
	if stop == batch.Errored {
		d.log.ErrorContext(ctx, "failed to list objects for purge",
			slog.String("backend", name), "error", err)
	}
}

// purgeOneObject drops one purged object's metadata row, logging deleteErr when
// its bytes could not be deleted from the backend. The item fails only when
// the row could not be dropped; a row already gone or replaced counts as done.
func (d *Manager) purgeOneObject(ctx context.Context, name string, obj *core.ObjectLocation, deleteErr error) batch.ItemResult {
	key := obj.ObjectKey
	if deleteErr != nil {
		d.log.WarnContext(ctx, "failed to delete object from backend during purge",
			slog.String("backend", name), slog.String("key", key), "error", deleteErr)
	}

	// The purge deleted the bytes itself, so the row goes without a cleanup.
	_, err := d.objects.DeleteObjectLocation(ctx, obj, 0, "")
	if err != nil && !errors.Is(err, core.ErrCopyChanged) {
		d.log.WarnContext(ctx, "failed to delete DB record during purge",
			slog.String("backend", name), slog.String("key", key), "error", err)
		return batch.ItemResult{Outcome: batch.ItemFailed, Status: progress.StatusFailed}
	}
	return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: progress.StatusOK}
}

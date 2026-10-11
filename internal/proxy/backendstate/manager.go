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
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
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

// Manager handles a backend's state record: draining and removing backends.
//
// states caches every backend's state from the records, so AcceptsWrites
// answers on the write path without a query. It is refreshed whenever this
// manager changes a record, on every usage-flush tick, and whenever the drainer
// reads the records. A stale entry only costs a ranking: admission reads the
// records itself.
type Manager struct {
	log     *slog.Logger
	infra   Runtime
	objects core.ObjectStore
	store   core.BackendStateStore
	states  atomic.Pointer[map[string]core.BackendState]
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

// AcceptsWrites reports whether the named backend takes new writes: it has no
// state record. A backend in any state, a drain that finished or failed
// included, is refused.
func (d *Manager) AcceptsWrites(name string) bool {
	_, ok := d.stateOf(name)
	return !ok
}

// IsDraining reports whether the named backend's state belongs to a drain:
// in progress, finished or failed.
func (d *Manager) IsDraining(name string) bool {
	state, ok := d.stateOf(name)
	return ok && state.IsDrain()
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

// SetStates replaces the cached states with the given records. The drainer
// hands it the records each pass reads.
func (d *Manager) SetStates(records []core.BackendStateRecord) {
	states := make(map[string]core.BackendState, len(records))
	for i := range records {
		states[records[i].BackendName] = records[i].State
	}
	d.states.Store(&states)
}

// stateOf returns the cached state of the named backend, and false when it has
// no record.
func (d *Manager) stateOf(name string) (core.BackendState, bool) {
	states := d.states.Load()
	if states == nil {
		return "", false
	}
	state, ok := (*states)[name]
	return state, ok
}

// -------------------------------------------------------------------------
// DRAIN
// -------------------------------------------------------------------------

// StartDrain records a drain for the backend, or restarts a failed one. From
// that moment admission refuses the backend, and the drainer moves its objects
// off on its next tick.
func (d *Manager) StartDrain(ctx context.Context, name string) error {
	if _, ok := d.infra.Backends()[name]; !ok {
		return fmt.Errorf("backend %q not found", name)
	}
	started, err := d.store.StartDrain(ctx, name)
	if err != nil {
		return err
	}
	if !started {
		return fmt.Errorf("backend %q is already draining or drained", name)
	}
	d.refreshAfterChange(ctx)
	audit.Log(ctx, "storage.DrainStart", slog.String("backend", name))
	d.log.InfoContext(ctx, "backend drain started", "backend", name)
	return nil
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
		Active:       rec.State == core.BackendStateDraining,
		State:        string(rec.State),
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

// CancelDrain clears the backend's state record, which makes it writable again.
// A drain in progress stops before its next page; objects already moved are
// not moved back.
func (d *Manager) CancelDrain(ctx context.Context, name string) error {
	cleared, err := d.store.ClearBackendState(ctx, name)
	if err != nil {
		return err
	}
	if !cleared {
		return fmt.Errorf("backend %q is not draining", name)
	}
	d.refreshAfterChange(ctx)
	audit.Log(ctx, "storage.DrainCancel", slog.String("backend", name))
	d.log.InfoContext(ctx, "backend drain cleared", "backend", name)
	return nil
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
	if rec != nil && rec.State == core.BackendStateDraining {
		return fmt.Errorf("backend %q is currently draining, cancel the drain first", name)
	}

	ctx = audit.WithRequestID(ctx, audit.NewID())

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
// the row could not be dropped.
func (d *Manager) purgeOneObject(ctx context.Context, name string, obj *core.ObjectLocation, deleteErr error) batch.ItemResult {
	key := obj.ObjectKey
	if deleteErr != nil {
		d.log.WarnContext(ctx, "failed to delete object from backend during purge",
			slog.String("backend", name), slog.String("key", key), "error", deleteErr)
	}

	_, err := d.objects.DeleteObjectLocation(ctx, key, name)
	if err != nil {
		d.log.WarnContext(ctx, "failed to delete DB record during purge",
			slog.String("backend", name), slog.String("key", key), "error", err)
		return batch.ItemResult{Outcome: batch.ItemFailed, Status: progress.StatusFailed}
	}
	return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: progress.StatusOK}
}
